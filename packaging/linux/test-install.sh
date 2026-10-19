#!/usr/bin/env bash
set -euo pipefail

[[ ${GITHUB_ACTIONS:-} == true && $EUID == 0 ]] || {
    echo "Requires root on a disposable CI runner." >&2; exit 1;
}
: "${VERSION:?}"
: "${COMMIT:?}"
: "${GOARCH:?}"
package=$(realpath "${1:?Supply a DEB or RPM}")
archive=$(realpath "${2:?Supply the matching tar.gz}")
[[ ! -e /usr/bin/dscregd && ! -e /etc/dscregd && ! -e /var/lib/dscregd ]] || {
    echo "Refusing to modify an existing dscregd installation or operator data." >&2; exit 1;
}
installed=false
cleanup() {
    if [[ $installed == true ]]; then
        if [[ $package == *.deb ]]; then dpkg --remove dscregd; else rpm -e dscregd; fi
    fi
}
trap cleanup EXIT
if [[ $package == *.deb ]]; then
    dpkg --install "$package"
    installed=true
    service=systemd
    systemctl is-active --quiet dscregd.service && exit 1
    systemctl is-enabled --quiet dscregd.service && exit 1
else
    rpm --install "$package"
    installed=true
    service=none
fi
python3 packaging/verify-installed.py --binary /usr/bin/dscregd --archive "$archive" \
    --root /var/lib/dscregd --config /etc/dscregd/config.json \
    --version "$VERSION" --commit "$COMMIT" --service "$service"
if [[ $package == *.deb ]]; then
    dpkg --purge dscregd
else
    rpm -e dscregd
fi
installed=false
[[ ! -e /usr/bin/dscregd && ! -e /usr/lib/systemd/system/dscregd.service ]]
[[ -f /var/lib/dscregd/registry.db && -f /etc/dscregd/config.json && -d /var/lib/dscregd/reports ]]
[[ -n $(find /var/lib/dscregd/reports -type f -print -quit) ]]
echo "Uninstall preserves operator data."
