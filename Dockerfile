FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/plugin .

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source=https://github.com/braghettos/oxide-rest-dynamic-controller-plugin
COPY --from=builder /bin/plugin /bin/plugin
EXPOSE 8080
ENTRYPOINT ["/bin/plugin"]
