FROM node:22-slim AS frontend-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS backend-build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/specrelay ./cmd/specrelay

# A real /bin/sh is intentionally present: project validation commands are the
# only user-configured commands that SpecRelay is allowed to execute via a shell.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/frontend /data
COPY --from=backend-build /out/specrelay /usr/local/bin/specrelay
COPY --from=frontend-build /src/frontend/dist/ /app/frontend/
ENV HTTP_ADDR=0.0.0.0:43846 \
    DATA_DIR=/data \
    PUBLIC_DIR=/app/frontend
VOLUME ["/data"]
EXPOSE 43846
ENTRYPOINT ["/usr/local/bin/specrelay"]
