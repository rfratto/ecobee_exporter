FROM golang:1.12-alpine as builder
RUN apk add --no-cache git
COPY . /src
WORKDIR /src
RUN go install .

FROM alpine
RUN apk add --no-cache ca-certificates
COPY --from=builder /go/bin/ecobee_exporter /bin/ecobee_exporter
ENTRYPOINT ["/bin/ecobee_exporter"]
