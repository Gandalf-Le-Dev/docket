FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/docket ./cmd/docket

FROM alpine:3.21
RUN adduser -D -H docket && mkdir -p /var/lib/docket && chown docket /var/lib/docket
COPY --from=build /out/docket /usr/local/bin/docket
USER docket
VOLUME /var/lib/docket
EXPOSE 8340
ENTRYPOINT ["docket"]
CMD ["serve"]
