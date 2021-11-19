ARG GOARCH="amd64"
# STEP 1: Build binary
FROM golang:1.17 AS builder
# golang envs
ARG GOARCH="amd64"
ARG GOOS=linux
ENV CGO_ENABLED=0
# copy in sources
WORKDIR /src
COPY . .
# build
RUN CGO_ENABLED=0 go build -o /go/bin/kcloudflarelb ./cmd
# STEP 2: Build small image
FROM cloudflare/cloudflared:2021.11.0
COPY --from=builder --chown=root:root /go/bin/kcloudflarelb /bin/kcloudflarelb
ENTRYPOINT ["/bin/kcloudflarelb"]