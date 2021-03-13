## Dockerfile for building production image
FROM golang:alpine
LABEL maintainer "Tim Smith <frompublic@timandjulz.com>"

FROM golang:alpine

# Set necessary environmet variables needed for our image
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

# Move to working directory /build
WORKDIR /build

# Copy and download dependency using go mod
#COPY go.mod .
#COPY go.sum .
COPY go.mod go.sum cjsocks.go /build/
RUN go mod download && go build -o cjsocks && cp /build/cjsocks /usr/local/bin/cjsocks

#ENV NODE_ENV=production \
#    PORT=80

#COPY cjsocks /usr/local/bin
#COPY pre-docker-entrypoint.sh /
#RUN apt update && apt install host jq docker-compose -y
#RUN npm install

EXPOSE 1085
CMD ["/usr/local/bin/cjsocks", "--autoadd=true", "--basedomain=cjsocks", "--port=1086"]
