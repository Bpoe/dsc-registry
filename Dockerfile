FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -o /dscregd ./cmd/dscregd

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && \
    mkdir /reports && chown 65532:65532 /reports && chmod 0700 /reports
COPY --from=build /dscregd /usr/local/bin/dscregd
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["dscregd"]
