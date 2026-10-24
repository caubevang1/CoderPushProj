# Image of the web demo (cmd/web). The CLI and the fake supplier are not
# needed in it: the web server embeds the fake supplier.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/web ./cmd/web

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H app
WORKDIR /app
COPY --from=build /out/web /app/web
COPY testdata/feeds /app/testdata/feeds
USER app
ENV PORT=8080 FEEDS_DIR=/app/testdata/feeds
EXPOSE 8080
ENTRYPOINT ["/app/web"]
