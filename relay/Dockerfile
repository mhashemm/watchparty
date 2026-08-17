FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /relay ./relay

FROM scratch
COPY --from=build /relay /relay
EXPOSE 80
ENTRYPOINT ["/relay"]
CMD ["-addr", ":80"]
