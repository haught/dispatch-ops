# syntax=docker/dockerfile:1

# The operator image (`docker build --target operator .`). Its stages come
# first so the server stays the default target.
FROM golang:1.27 AS operator-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/dispatch-operator ./cmd/dispatch-operator
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dispatch-operator ./cmd/dispatch-operator

FROM gcr.io/distroless/static-debian12:nonroot AS operator
COPY --from=operator-build /out/dispatch-operator /usr/local/bin/dispatch-operator
# distroless's nonroot user, by number so Kubernetes can enforce runAsNonRoot.
USER 65532:65532
ENTRYPOINT ["dispatch-operator"]

# The server image.
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
# distroless's nonroot user, by number so Kubernetes can enforce runAsNonRoot.
USER 65532:65532
ENTRYPOINT ["dispatch-ops"]
CMD ["serve"]
