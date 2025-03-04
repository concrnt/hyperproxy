FROM ubuntu:noble AS corebuilder
WORKDIR /work

ARG VERSION

RUN apt update && apt install -y golang-go libmagick++-6.q16-dev

COPY ./go.mod ./go.sum ./
RUN go mod download && go mod verify
COPY ./ ./
RUN VERSION=${VERSION:-$(git describe)} \
    BUILD_MACHINE=$(uname -srmo) \
    BUILD_TIME=$(date) \
    GO_VERSION=$(go version) \
    CGO_CPPFLAGS="$(pkg-config --cflags Magick++)" \
    CGO_LDFLAGS="$(pkg-config --libs Magick++)" \
    go build -ldflags "-s -w -X main.version=${VERSION} -X \"main.buildMachine=${BUILD_MACHINE}\" -X \"main.buildTime=${BUILD_TIME}\" -X \"main.goVersion=${GO_VERSION}\"" -o hyperproxy

FROM ubuntu:noble
RUN apt-get update \
 && apt-get install -y ca-certificates curl libmagickcore-6.q16-7t64 libmagick++-6.q16-9t64 libmagickwand-6.q16-7t64 ffmpeg --no-install-recommends \
 && rm -rf /var/lib/apt/lists/*

COPY --from=corebuilder /work/hyperproxy /usr/local/bin

CMD ["hyperproxy"]
