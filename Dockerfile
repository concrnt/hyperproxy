FROM ubuntu:noble AS corebuilder
WORKDIR /work

ARG VERSION

RUN apt update && apt install -y golang-go libvips-dev

COPY ./go.mod ./go.sum ./
RUN go mod download && go mod verify
COPY ./ ./
RUN VERSION=${VERSION:-$(git describe)} \
    BUILD_MACHINE=$(uname -srmo) \
    BUILD_TIME=$(date) \
    GO_VERSION=$(go version) \
    go build -ldflags "-s -w -X main.version=${VERSION} -X \"main.buildMachine=${BUILD_MACHINE}\" -X \"main.buildTime=${BUILD_TIME}\" -X \"main.goVersion=${GO_VERSION}\"" -o hyperproxy

FROM ubuntu:noble AS runtime
RUN apt-get update \
 && apt-get install -y ca-certificates curl libvips42t64 ffmpeg \
      libheif-plugin-libde265 libheif-plugin-dav1d \
      --no-install-recommends \
 && rm -rf /var/lib/apt/lists/*

COPY --from=corebuilder /work/hyperproxy /usr/local/bin

CMD ["hyperproxy"]
