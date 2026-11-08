# syntax=docker/dockerfile:1

FROM node:25-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./web/build
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dispatch-ops ./cmd/dispatch-ops

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dispatch-ops /usr/local/bin/dispatch-ops
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["dispatch-ops"]
CMD ["serve"]
