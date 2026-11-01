# Small image: just the one program, no operating system inside.
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/flameboss-hub .

FROM scratch
COPY --from=build /out/flameboss-hub /flameboss-hub
# Settings and cook history live in /data; keep it on a volume so it survives updates.
ENV FLAMEBOSS_DATA=/data FLAMEBOSS_LISTEN=0.0.0.0:8075
VOLUME /data
LABEL net.unraid.docker.icon="https://raw.githubusercontent.com/jsttylr/flameboss-hub/main/unraid/icon.png" \
      net.unraid.docker.webui="http://[IP]:[PORT:8075]/" \
      org.opencontainers.image.source="https://github.com/jsttylr/flameboss-hub" \
      org.opencontainers.image.description="Local replacement for the Flame Boss / EGG Genius app: live grill page, cook history, Home Assistant bridge. No cloud." \
      org.opencontainers.image.licenses="MIT"
# 8075 = web page, 1883 = built-in broker for Home Assistant
EXPOSE 8075/tcp 1883/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/flameboss-hub", "-healthcheck"]
ENTRYPOINT ["/flameboss-hub"]
