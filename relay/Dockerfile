FROM golang:1.27.3-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /relay ./relay
# scratch has no shell to make it in; a new volume takes this owner from the image
RUN mkdir /data && chown 65534:65534 /data

FROM scratch
COPY --from=build /relay /relay
COPY --from=build --chown=65534:65534 /data /data
USER 65534:65534
ENV ADDR=":80" \
    CREATE_KEY="" \
    TRUST_PROXY="" \
    ROOMS_PER_HOUR="" \
    MAX_ROOMS="" \
    MAX_CONNS="" \
    MAX_WAITING="" \
    DB="/data/relay.db"
VOLUME /data
EXPOSE 80
ENTRYPOINT ["/relay"]
