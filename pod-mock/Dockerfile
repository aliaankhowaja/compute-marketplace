FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pod-mock/go.mod pod-mock/go.sum ./
RUN go mod download
COPY pod-mock/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pod-mock .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /pod-mock /usr/local/bin/pod-mock
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/pod-mock"]
