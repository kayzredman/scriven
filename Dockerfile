# syntax=docker/dockerfile:1
# Scriven container image: the CLI, Terraform, and the pinned Kubernetes
# provider baked in, so captures need no downloads at run time.
#
#   docker build -t scriven:dev .
#
# Runs as a non-root user and works with a read-only root filesystem
# (give it a writable /tmp).

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.22

# --- build the CLI ------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scriven ./cmd/scriven

# --- Terraform and the provider -----------------------------------------------
FROM alpine:${ALPINE_VERSION} AS terraform
ARG TERRAFORM_VERSION=1.17.1
ARG KUBERNETES_PROVIDER_VERSION=3.5.0
ARG TARGETARCH=amd64
RUN apk add --no-cache curl unzip
WORKDIR /tmp/tf
# Verify the download against HashiCorp's published checksums.
RUN curl -fsSLO "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip" \
 && curl -fsSLO "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_SHA256SUMS" \
 && grep "linux_${TARGETARCH}.zip" "terraform_${TERRAFORM_VERSION}_SHA256SUMS" | sha256sum -c - \
 && unzip -q "terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip" -d /usr/local/bin
# Pre-fetch the provider. The unpacked layout doubles as a filesystem mirror, which
# Terraform reads without writing to it (a plugin cache gets rewritten when there
# is no lock file, which fails on a read-only root filesystem).
RUN mkdir -p /opt/scriven/plugins /tmp/prefetch \
 && printf 'terraform {\n  required_providers {\n    kubernetes = {\n      source  = "hashicorp/kubernetes"\n      version = "= %s"\n    }\n  }\n}\n' "${KUBERNETES_PROVIDER_VERSION}" > /tmp/prefetch/main.tf \
 && TF_PLUGIN_CACHE_DIR=/opt/scriven/plugins terraform -chdir=/tmp/prefetch init -input=false -no-color >/dev/null
COPY <<EOF /opt/scriven/terraformrc
provider_installation {
  filesystem_mirror {
    path    = "/opt/scriven/plugins"
    include = ["registry.terraform.io/hashicorp/kubernetes"]
  }
  direct {
    exclude = ["registry.terraform.io/hashicorp/kubernetes"]
  }
}
EOF

# --- runtime ------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache git ca-certificates \
 && adduser -D -H -u 10001 -s /sbin/nologin scriven
COPY --from=terraform /usr/local/bin/terraform /usr/local/bin/terraform
COPY --from=terraform /opt/scriven /opt/scriven
COPY --from=build /out/scriven /usr/local/bin/scriven
ENV HOME=/tmp \
    TMPDIR=/tmp \
    TF_CLI_CONFIG_FILE=/opt/scriven/terraformrc \
    CHECKPOINT_DISABLE=1
USER 10001
ENTRYPOINT ["scriven"]
CMD ["--help"]
