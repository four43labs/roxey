FROM golang:1.26-alpine AS build
WORKDIR /src
RUN mkdir /data
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/roxey-relay .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build --chown=nonroot /data /data
COPY --from=build /out/roxey-relay /app/roxey-relay
COPY --from=build /src/web /app/web

ENV ROXEY_DB_PATH=/data/roxey.db
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/app/roxey-relay"]
