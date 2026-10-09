FROM golang:1.26.9-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api \
    && CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 go build -trimpath -o /out/receiver ./cmd/receiver \
    && CGO_ENABLED=0 go build -trimpath -o /out/admin ./cmd/admin

FROM build AS loadtest-build
RUN CGO_ENABLED=0 go build -trimpath -o /out/loadtest ./cmd/loadtest

FROM alpine:3.23 AS runtime
RUN apk upgrade --no-cache && adduser -D -u 10001 app
USER app

FROM runtime AS loadtest
COPY --from=loadtest-build /out/loadtest /usr/local/bin/loadtest
ENTRYPOINT ["loadtest"]

FROM runtime AS api
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/admin /usr/local/bin/admin
ENTRYPOINT ["api"]

FROM runtime AS worker
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["worker"]

FROM runtime AS receiver
COPY --from=build /out/receiver /usr/local/bin/receiver
ENTRYPOINT ["receiver"]
