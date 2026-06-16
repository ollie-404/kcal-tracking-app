FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/foodtrack ./cmd/foodtrack

FROM alpine:3.20
RUN adduser -D -H app && apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/foodtrack /app/foodtrack
COPY templates /app/templates
COPY static /app/static
USER app
EXPOSE 8080
ENTRYPOINT ["/app/foodtrack"]
