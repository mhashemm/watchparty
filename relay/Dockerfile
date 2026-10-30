FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /relay ./relay

FROM scratch
COPY --from=build /relay /relay
ENV ADDR=":80" \
    CREATE_KEY="" \
    TRUST_PROXY="" \
    ROOMS_PER_HOUR="" \
    MAX_ROOMS="" \
    MAX_CONNS=""
EXPOSE 80
ENTRYPOINT ["/relay"]
