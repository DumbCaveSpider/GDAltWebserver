FROM golang:1.21-alpine AS builder
WORKDIR /build
COPY . .
RUN go build -o gdaltweb ./services
 
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /build/gdaltweb .
CMD ["./gdaltweb"]