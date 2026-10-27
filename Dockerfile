FROM golang:1.27.2-alpine AS builder

WORKDIR /app
COPY . /app
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -tags netgo,opusergo -a -trimpath -ldflags='-s -w -extldflags "-static" -buildid=' -o main .

# Pinned restic release, downloaded once at build time and verified against
# the official SHA256SUMS. To update: bump RESTIC_VERSION and replace the two
# checksums with the linux_amd64/linux_arm64 lines from
# https://github.com/restic/restic/releases/download/v<version>/SHA256SUMS
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256_AMD64=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c
ARG RESTIC_SHA256_ARM64=a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465
RUN apk add --no-cache curl bzip2 \
    && case "${TARGETARCH:-$(uname -m)}" in \
        amd64|x86_64) restic_arch="amd64"; expected="${RESTIC_SHA256_AMD64}" ;; \
        arm64|aarch64) restic_arch="arm64"; expected="${RESTIC_SHA256_ARM64}" ;; \
        *) echo "unsupported architecture: ${TARGETARCH:-$(uname -m)}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/restic.bz2 "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_${restic_arch}.bz2" \
    && echo "${expected}  /tmp/restic.bz2" | sha256sum -c - \
    && bunzip2 /tmp/restic.bz2 \
    && chmod 0755 /tmp/restic

FROM ghcr.io/greboid/dockerbase/root:1.20261020.1

COPY --from=builder /app/main /bailer
COPY --from=builder /tmp/restic /usr/local/bin/restic
ENTRYPOINT ["/bailer"]
