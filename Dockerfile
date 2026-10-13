FROM golang:1.27 AS build
WORKDIR /root/go/src/github.com/nalind/chrooty
COPY . /root/go/src/github.com/nalind/chrooty/
RUN go build -ldflags "-w -s"
FROM scratch
COPY --from=build //root/go/src/github.com/nalind/chrooty/chrooty /chrooty
