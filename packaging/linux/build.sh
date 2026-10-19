#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
: "${VERSION:?Set VERSION to vMAJOR.MINOR.PATCH}"
: "${COMMIT:?Set COMMIT to the full source commit}"
: "${GOARCH:?Set GOARCH to amd64 or arm64}"
[[ $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "VERSION must be a stable vMAJOR.MINOR.PATCH tag." >&2; exit 1;
}
[[ $COMMIT =~ ^[0-9a-f]{40}$ ]] || {
    echo "COMMIT must be the full source commit." >&2; exit 1;
}
[[ $GOARCH == amd64 || $GOARCH == arm64 ]] || {
    echo "GOARCH must be amd64 or arm64." >&2; exit 1;
}
output=$(realpath -m "${1:?Supply an output directory}")
mkdir -p "$output"
export STAGE_DIR
STAGE_DIR=$(mktemp -d)
trap 'rm -rf "$STAGE_DIR"' EXIT
CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
    -o "$STAGE_DIR/dscregd" ./cmd/dscregd
# Packaged units use /usr/bin; the manual archive retains /usr/local/bin.
sed 's@/usr/local/bin/dscregd@/usr/bin/dscregd@' deploy/dscregd.service > "$STAGE_DIR/dscregd.service"
rpm_arch=x86_64
[[ $GOARCH == amd64 ]] || rpm_arch=aarch64
nfpm package --config packaging/linux/nfpm.yaml --packager deb \
    --target "$output/dscregd_${VERSION#v}_${GOARCH}.deb"
nfpm package --config packaging/linux/nfpm.yaml --packager rpm \
    --target "$output/dscregd-${VERSION#v}-1.$rpm_arch.rpm"
name="dscregd-$VERSION-linux-$GOARCH"
package="$STAGE_DIR/$name"
mkdir -p "$package/bin" "$package/deploy" "$package/packaging/linux"
cp "$STAGE_DIR/dscregd" "$package/bin/"
cp README.md LICENSE "$package/"
cp -R docs "$package/"
cp deploy/dscregd.service "$package/deploy/"
cp packaging/linux/config.example.json "$package/packaging/linux/"
tar -czf "$output/$name.tar.gz" -C "$STAGE_DIR" "$name"
(cd "$output" && sha256sum --text "$name.tar.gz" "dscregd_${VERSION#v}_${GOARCH}.deb" \
    "dscregd-${VERSION#v}-1.$rpm_arch.rpm" > SHA256SUMS)
