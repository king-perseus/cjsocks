package main

// Based on https://github.com/asjustas/docker-resolver
// TODO: Command line parameters are not being parsed.  Using default ports.
// TODO: Switch from socks5 implementation to coreDNS + socks5 + host file updater.  This will enable using DNS or hosts file for non MacOS

/* Documentation:
cjsocks provides a socks5 implementation that runs inside a container.  Configure your
browser to route http traffic to container through the socks5 server.  cjsocks looks up domain
names in its cache and directs traffic.

cjsocks uses Container Jockey labels on containers to determine what the
Fully Qualified Domain Names (FQDN) are.  These labels are optional.
By default, FQDN is generated bu adding the container name and a configurable domain name.
The default domain name is "container".  So a container named "myservice"
will get a FQDN "myservice.container" if nothing else is configured.

Containers created by docker-compose automatically get a subdomain.  So a container
named "myservice" created in a docker-compose project "myproject" will get
a FQDN "myservice.myproject.container"

Requirements:
- Containers must have a network in common with the cjsocks container for requests
  to route to the containers.

The implementation does the following:
- Creates a docker network "cj-socks5" if it doesn't already exist
- Creates a socks5 proxy listening on a configured port (default 1085)
- Provides DNS resolution via a custom socks5 resolver
- Monitors container creation/destruction to add/remove DNS entries
- To ensure connectivity, new containers are automatically added to the cj-socks

*/

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/chuckpreslar/emission"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/haxii/socks5"
)

const default_ip string = "0.0.0.0"
const default_auto_add_to_cjnetwork bool = false
const default_port string = "1085"
const listen_protocol string = "tcp"
const default_base_domain string = "container" // Default domain for the containers.  e.g. hostname.container
const default_cj_network_name string = "cj-socks5"

const label_cj_hostname string = "org.cj-tools.hosts.host_name"
const label_docker_compose_service string = "com.docker.compose.service"
const label_docker_compose_project string = "com.docker.compose.project"
const label_cj_subdomain string = "org.cj-tools.hosts.sub_domain"
const label_cj_domain string = "org.cj-tools.hosts.domain_name"
const label_cj_flag_use_container_base_domain string = "org.cj-tools.hosts.use_container_base_domain"

type App struct {
	emitter               *emission.Emitter
	fqdnToIp              map[string]string // Resolve a lower case DNS name to an IP address
	defaultBaseDomain     string
	cjnetworkName         string // containers with cj labels get added here automatically if they don't already exist on the network
	auto_add_to_cjnetwork bool
}

type BindFlags []string

func main() {
	app := new(App)
	app.emitter = emission.NewEmitter()
	app.cjnetworkName = default_cj_network_name
	app.fqdnToIp = make(map[string]string)
	// TODO: Create the network name if it doesn't already exist.  Include labels.

	b, _ := strconv.ParseBool(os.Getenv("CJ_AUTO_ADD"))
	app.auto_add_to_cjnetwork = *flag.Bool("autoadd", b, "Default base domain for containers if not overridden")

	app.defaultBaseDomain = *flag.String("basedomain", os.Getenv("CJ_BASE_DOMAIN"), "Default base domain for containers if not overridden")
	if app.defaultBaseDomain == "" {
		app.defaultBaseDomain = default_base_domain
	}

	// Options:
	// Start socks5 server on IP:port.
	ip := os.Getenv("CJ_LISTEN_IP")
	flag.String("listenip", ip, "IP address to start the socks5 server on")
	if ip == "" {
		ip = default_ip
	}
	bindip := net.ParseIP(ip)

	bp := os.Getenv("CJ_SOCKS_PORT")
	flag.String("port", bp, "Port to listen on")
	if bp == "" {
		bp = default_port
	}
	bindport, _ := strconv.Atoi(bp)

	containerStart := func(domains []string, ip string) {
		fmt.Printf("ContainerStart %s\n%s\n\n", domains, ip)
	}

	containerStop := func(domains []string) {
		fmt.Printf("ContainerStop %s\n\n", domains)
	}

	app.emitter.On("container-start", containerStart)
	app.emitter.On("container-stop", containerStop)

	fmt.Println("Creating resolver")

	// resolver := socks5.CJResolver{}
	resolver := app

	fmt.Println("Creating conf")

	conf := socks5.Config{
		Resolver: resolver,
		BindIP:   bindip,
		BindPort: bindport,
	}

	fmt.Println("Creating socks server")
	// This populates conf with defaults if I didn't provide a value.
	server, err := socks5.New(&conf)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Starting socks5 server on %v\n", net.IP.String(conf.BindIP)+":"+strconv.Itoa(conf.BindPort))

	go app.monitorDocker()

	// Start the socks5 server
	// For some reason I have to specify the protocol, address and port even though conf has it.
	// TODO: Add a check for data:EADDRINUSE  (address in use).  Retry some period of time.
	if err := server.ListenAndServe(listen_protocol, net.IP.String(conf.BindIP)+":"+strconv.Itoa(conf.BindPort)); err != nil {
		panic(err)
	}

}

func (app *App) monitorDocker() {
	// Monitors a channel of docker events
	fmt.Println("Starting docker events listener")

	client, err := docker.NewClient("unix:///var/run/docker.sock")

	if err != nil {
		panic(err)
	}

	// Create the network if it doesn't exist
	network_options := docker.CreateNetworkOptions{
		Name:           app.cjnetworkName,
		Labels:         map[string]string{"description": "Default network used by cj-socks to bridge communication to other containers."},
		CheckDuplicate: true,
		Attachable:     true,
	}
	fmt.Println("Creating network in case it does not already exist", app.cjnetworkName)
	_, e := client.CreateNetwork(network_options)
	if e != nil {
		fmt.Printf("WARNING: Could not create network %v %T %#v\n", app.cjnetworkName, e, e)
		// TODO: Need to figure out how to extract the docker error structure from the error
		//var dockererror *docker.Error = &e
		/*
			if e.Status == 409 {
				// Network already existed
				panic(e)
			} else {
				panic(e)
			}
		*/
	}

	registerRunningContainers(app, client)

	events := make(chan *docker.APIEvents)
	err = client.AddEventListener(events)
	if err != nil {
		panic(err)
	}

	defer client.RemoveEventListener(events)

	// Loops constantly on events
	for event := range events {
		action := strings.Split(event.Action, ":")[0] // Some actions include details.  But most are just the word.
		switch action {
		case "exec_create", "exec_start", "exec_die":
		case "create":
			fmt.Printf("Event [%v]\n", event.Action)
			if app.auto_add_to_cjnetwork {
				container, _ := client.InspectContainer(event.ID)

				// Check if the container is already in our targeted socks network
				// or one of the networks attached to this (the cj-socks) container
				for networkname, net := range container.NetworkSettings.Networks {
					fmt.Printf("Network %v = %v %#v", networkname, net.IPAddress, net)
				}

				opts := docker.NetworkConnectionOptions{
					Container: container.ID,
					Force:     false,
				}
				fmt.Println("Connecting network cj-proxy")
				client.ConnectNetwork(app.cjnetworkName, opts)
			}
		case "start":
			fmt.Printf("Event [%v]\n", event.Action)
			container, _ := client.InspectContainer(event.ID)

			fmt.Printf("\nLabels: %#v\n", container.Config.Labels)
			ip := getContainerIP(app, client, event.ID)
			domains := getDomains(client, event.ID, app.defaultBaseDomain)
			app.registerDomains(domains, ip)
			/*
				fmt.Printf("Got docker events Action [%v]\n%%#v=%#v\n %%v=%v\n\n", event.Action, event, event)
				domains := getDomains(client, event.ID, app)
				// TODO: If the container has a cj label then automatically add it to the cj network.
				// TODO: If container is added/removed on cj-network then update domain names list
				ip := getContainerIP(client, event.ID)
				app.registerDomains(domains, ip)
				app.emitter.Emit("container-start", domains, ip)
				app.emitter.Emit("domains-updated")
			*/
		// Also: "destroy" when container deleted and "disconnect" when stopped/removed from network
		case "destroy", "stop", "kill", "die":
			fmt.Printf("Event [%v]\n", event.Action)
			/*
				fmt.Printf("Got docker events Action [%v]\n%%#v=%#v\n %%v=%v\n\n", event.Action, event, event)
				domains := getDomains(client, event.ID)
				app.removeDomains(domains)
				app.emitter.Emit("container-stop", domains)
				app.emitter.Emit("domains-updated")
			*/
		case "disconnect": // Disconnected from a network.  Container may not be running!
			// Disconnect event fires when container is stopped or removed from network.
			// However the IP address has been disposed at this point
			fmt.Printf("Event [%v]\n%%#v=%#v\n", event.Action, event)
		case "connect": // Connected to a network.  Only fires when container starts or is running.
			// NOTE: IP Address is not available at time of connect.
			fmt.Printf("Event [%v]\n%%#v=%#v\n", event.Action, event)
		default:
			fmt.Printf("Unhandled event [%v]\n", event.Action)
			// fmt.Printf("Got docker events Action [%v]\n%%#v=%#v\n %%v=%v\n\n", event.Action, event, event)
		}
	}
}

func (app *App) registerDomains(domains []string, ip string) {
	if ip == "" {
		return
	}
	for _, fqdn := range domains {
		// app.records[domain] = ip
		fmt.Printf("\t[%v] [%v]\n", fqdn, ip)
		app.fqdnToIp[fqdn] = ip
	}
}

func (app *App) removeDomains(domains []string) {
	for _, domain := range domains {
		delete(app.fqdnToIp, domain)
	}
}

func getContainerIP(app *App, client *docker.Client, ID string) string {
	// WARNING: A blank IP address can get returned for some containers exposed only on the host network adapter.
	// IP Address exposed inside the Docker network.  Or host IP if not exposed on the Docker network.
	// IP priority order:
	// - If connected to the network named app.cjnetworkName, its IP address
	// - If connected to another docker network, the first IP address found
	// - The address on the first attached network (could be blank if only connected on Host network)
	// - "HostIp" if the container is exposed on the host network
	container, _ := client.InspectContainer(ID)

	var ip string
	var firstip string

	// Check if the container is already in our targeted socks network
	// or one of the networks attached to this (the cj-socks) container
	for networkname, net := range container.NetworkSettings.Networks {
		if firstip == "" {
			firstip = net.IPAddress
		}
		// fmt.Printf("Network %v = %v %#v", networkname, net.IPAddress, net)
		if strings.ToLower(networkname) == app.cjnetworkName {
			ip = net.IPAddress
			break
		}
	}

	if ip == "" {
		ip = firstip
	}

	if ip == "" {
		for _, bindings := range container.NetworkSettings.Ports {
			for _, b := range bindings {
				ip = b.HostIP
				if ip != "" {
					break
				}
			}
		}
	}

	return ip
}

// Resolve ...
func (app App) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	fmt.Printf("Custom resolver called for %s\n", name)

	var addr *net.IPAddr
	var err error
	if ip := app.fqdnToIp[name]; ip != "" {
		addr, err = net.ResolveIPAddr("ip", ip)
	} else {
		addr, err = net.ResolveIPAddr("ip", name)
	}
	if err != nil {
		fmt.Printf("Got an error %s\n", name)
		return ctx, nil, err
	}
	fmt.Printf("Returning address %s\n", net.IP.String(addr.IP))
	return ctx, addr.IP, err
}

func registerRunningContainers(app *App, client *docker.Client) {
	fmt.Println("Registering running containers")

	containers, err := client.ListContainers(docker.ListContainersOptions{})

	if err != nil {
		panic(err)
	}
	for _, container := range containers {
		domains := getDomains(client, container.ID, app.defaultBaseDomain)
		ip := getContainerIP(app, client, container.ID)

		app.registerDomains(domains, ip)
	}

	app.emitter.Emit("domains-updated")
}

func getDomains(client *docker.Client, ID string, defaultBaseDomain string) []string {
	domains := []string{}
	container, _ := client.InspectContainer(ID)

	// Private host name
	// service_hostname := container.Config.Labels[label_docker_compose_service]

	// Public host name
	// Order of precedence: Host name label, docker service name, container host name, container name
	public_hostname := container.Config.Labels[label_cj_hostname]
	if public_hostname == "" {
		public_hostname = container.Config.Labels[label_docker_compose_service]
	}
	if public_hostname == "" {
		// Docker will automatically generate a 12 character host name if none is configured
		// TODO: Be a little more creative than just checking name length = 12.  Maybe regex replace [a-f0-9] and verify all 12 characters are a match.
		if len(container.Config.Hostname) != 12 && container.Config.Hostname > "" {
			public_hostname = container.Config.Hostname
		} else {
			public_hostname = container.Name[1:] // Skip the leading slash
		}
	}

	// --- FQDN
	fqdn := public_hostname + "."
	// --- Full domain name
	//     Order of precedence:
	//       If label says then only use the container domain name.
	//       otherwise
	//       Subdomain label or compose project + label domain or container domain name or external configured domain
	if container.Config.Labels[label_cj_flag_use_container_base_domain] == "true" && container.Config.Domainname > "" {
		fqdn = public_hostname + "." + container.Config.Domainname
	} else {
		// --- or Sub domain + Base domain name
		fqdn = public_hostname + "."
		if container.Config.Labels[label_cj_subdomain] != "" {
			fqdn += container.Config.Labels[label_cj_subdomain] + "."
		} else if container.Config.Labels[label_docker_compose_project] > "" {
			fqdn += container.Config.Labels[label_docker_compose_project] + "."
		}
		if container.Config.Labels[label_cj_domain] != "" {
			fqdn += container.Config.Labels[label_cj_domain]
		} else if container.Config.Labels[label_cj_flag_use_container_base_domain] == "true" && container.Config.Domainname != "" {
			fqdn += container.Config.Domainname
		} else {
			fqdn += defaultBaseDomain
		}

	}
	domains = append(domains, fqdn)

	/*
		if "" != container.Config.Domainname {
			domains := append(domains, container.Config.Hostname+"."+container.Config.Domainname)
		}
	*/

	//domains = append(domains, container.Name[1:]+".docker")
	/*
		envDomains := getDomainsFromEnv(container.Config.Env)

		for _, domain := range envDomains {
			domains = append(domains, domain)
		}
	*/

	return domains
}

// Parameters:
// Network IP address to listen on.  Default "0.0.0.0"
// Port for socks5 to listen on.  Default 1085
// Name of docker network to expose.  Default cjnetwork

// Steps:
// Get IP address for targetted networks first, then default network.
// Add/remove DNS to IP address
// Build executable and container.  Test on Mac
// LATER: Add optional DNS server
