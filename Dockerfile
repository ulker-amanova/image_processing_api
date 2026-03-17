FROM golang:1.22-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod .
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /image-service ./main.go

FROM scratch
COPY --from=builder /image-service /image-service
EXPOSE 8080
ENTRYPOINT ["/image-service"]
