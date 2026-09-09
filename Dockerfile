FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api \
    && CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 go build -trimpath -o /out/receiver ./cmd/receiver

FROM alpine:3.22 AS api
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/api /usr/local/bin/api
ENTRYPOINT ["api"]

FROM alpine:3.22 AS worker
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["worker"]

FROM alpine:3.22 AS receiver
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/receiver /usr/local/bin/receiver
ENTRYPOINT ["receiver"]
