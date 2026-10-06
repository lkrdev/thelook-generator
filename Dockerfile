FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/ internal/
COPY main.go cloud.go seed_data.json ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /thelook .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /data
COPY --from=builder /thelook /usr/local/bin/thelook
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/thelook"]
CMD ["run", "--state", "/data/state.gob", "--profile", "/data/profile.json"]
