#!/bin/sh
# POSIX sh installer. Never invoke with sh -x: enrollment input is secret.
set -eu
# Even if a caller accidentally enabled xtrace, do not allow secret handling
# below to be traced. This is intentionally never re-enabled.
set +x
umask 077
LC_ALL=C
export LC_ALL

say() { printf '%s\n' "[orbitbeacon] $*"; }
die() { printf '%s\n' "[orbitbeacon] ERROR: $*" >&2; exit 1; }
usage() {
    cat <<'USAGE'
Usage: sh install-agent.sh --server http[s]://HOST[:PORT] --version VERSION
       [--ca-file FILE] [--token-file FILE] [--binary FILE]
       sh install-agent.sh --uninstall [--yes]
Run as root. --binary replaces downloading (then --version is optional).
Supply the enrollment token through a file, stdin, or the hidden terminal prompt.
Tokens are never accepted as command-line arguments. Existing identity is kept.
USAGE
}

server='' version='' ca_source='' token_source='' local_binary=''
uninstall=0 yes=0
while [ "$#" -gt 0 ]; do
    case $1 in
        --server|--version|--ca-file|--token-file|--binary)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "Missing value for $1"; fi
            case $1 in
                --server) server=$2 ;;
                --version) version=$2 ;;
                --ca-file) ca_source=$2 ;;
                --token-file) token_source=$2 ;;
                --binary) local_binary=$2 ;;
            esac
            shift 2 ;;
        --uninstall) uninstall=1; shift ;;
        --yes) yes=1; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die 'Unknown option. Use --help; pass tokens only through stdin or --token-file.' ;;
    esac
done

# TEST HOOKS ONLY: both opt-in variables and a sentinel are mandatory. Normal
# installs always use real absolute paths. The harness supplies command stubs.
root=
if [ "${ORBITBEACON_INSTALL_TEST:-}" = 1 ]; then
    root=${ORBITBEACON_INSTALL_ROOT:-}
    case $root in /tmp/*) ;; *) die 'Test root must be a private directory under /tmp.' ;; esac
    case $root in *[!A-Za-z0-9_./-]*) die 'Unsafe test root.' ;; esac
    if [ ! -d "$root" ] || [ ! -f "$root/.orbitbeacon-install-test-root" ]; then die 'Missing test root sentinel.'; fi
    [ "$(cd "$root" && pwd -P)" = "$root" ] || die 'Test root must be canonical and contain no symlinks.'
elif [ -n "${ORBITBEACON_INSTALL_ROOT:-}" ]; then
    die 'A root prefix is only supported by the guarded test harness.'
fi
[ "$(id -u)" = 0 ] || die 'Run this installer as root.'
[ "$(uname -s)" = Linux ] || die 'Only Linux is supported.'

account=orbitbeacon-agent
bin_dir=$root/usr/local/bin
binary=$bin_dir/orbitbeacon-agent
config_dir=$root/etc/orbitbeacon
policy=$config_dir/install-policy.json
runtime_config=$config_dir/agent.json
ca_copy=$config_dir/ca.pem
state=$root/var/lib/orbitbeacon-agent
token_file=$state/enrollment.token
unit_dir=$root/etc/systemd/system
unit=$unit_dir/orbitbeacon-agent.service
tmp='' policy_tmp='' binary_tmp='' unit_tmp='' tty_state=''
cleanup() {
    if [ -n "$tty_state" ]; then stty "$tty_state" </dev/tty || :; fi
    [ -z "$policy_tmp" ] || rm -f -- "$policy_tmp"
    [ -z "$binary_tmp" ] || rm -f -- "$binary_tmp"
    [ -z "$unit_tmp" ] || rm -f -- "$unit_tmp"
    [ -z "$tmp" ] || rm -rf -- "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

# Refuse symlinks in destinations and their ancestors, including during removal.
safe_path() {
    checked=$1
    while [ "$checked" != / ]; do
        [ ! -L "$checked" ] || die "Refusing symbolic link: $checked"
        checked=${checked%/*}
        [ -n "$checked" ] || checked=/
    done
}
for path in "$binary" "$policy" "$runtime_config" "$ca_copy" "$token_file" "$state/identity.json" "$unit"; do
    safe_path "$path"
done
has_systemd() {
    command -v systemctl >/dev/null 2>&1 && [ -d "$root/run/systemd/system" ]
}

if [ "$uninstall" = 1 ]; then
    [ -z "$server$version$ca_source$token_source$local_binary" ] || die 'Do not combine install options with --uninstall.'
    say 'Revoke this node credential in the console first; local deletion does not revoke it.'
    say 'Uninstall removes the service, binary, policy, local configuration, CA copy and ALL agent state. Stop any foreground agent first.'
    if [ "$yes" != 1 ]; then
        printf 'Type yes to permanently remove this installation: ' >&2
        answer=
        read -r answer || :
        [ "$answer" = yes ] || die 'Uninstall cancelled; no files removed.'
    fi
    if has_systemd; then
        # A unit can remain loaded/enabled after a partial previous uninstall.
        # A missing unit is harmless; continue removing local files.
        systemctl disable --now orbitbeacon-agent >/dev/null 2>&1 || :
    fi
    rm -f -- "$unit" "$binary" "$policy" "$runtime_config" "$ca_copy"
    rm -rf -- "$state"
    if has_systemd; then systemctl daemon-reload; fi
    say 'Uninstalled. The system account is retained; revoke the server credential if not already done.'
    exit 0
fi
[ "$yes" = 0 ] || die '--yes is only valid with --uninstall.'
[ ! -e "$runtime_config" ] || [ -f "$runtime_config" ] || die 'Local agent configuration must be a regular file.'
[ -n "$server" ] || die '--server is required.'
# A conservative ASCII origin grammar also guarantees JSON and shell safety.
# Permit a single root slash and normalize it away. No userinfo, path, query,
# fragment, whitespace, quote, backslash, zone ID or percent-encoded host.
server=${server%/}
if ! printf '%s\n' "$server" | awk '
    function valid6(s, a, b, i, n, c) {
        if (s !~ /^[0-9A-Fa-f:]+$/ || s ~ /:::/) return 0
        if (s ~ /::/) {
            if (split(s, a, /::/) != 2) return 0
            n = 0
            if (a[1] != "") { c = split(a[1], b, ":"); for (i = 1; i <= c; i++) { if (length(b[i]) > 4) return 0; n++ } }
            if (a[2] != "") { c = split(a[2], b, ":"); for (i = 1; i <= c; i++) { if (length(b[i]) > 4) return 0; n++ } }
            return n < 8
        }
        c = split(s, b, ":")
        if (c != 8) return 0
        for (i = 1; i <= c; i++) if (length(b[i]) < 1 || length(b[i]) > 4) return 0
        return 1
    }
    NR != 1 || $0 !~ /^https?:\/\/([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]+)?$/ { exit 1 }
    { host=$0; sub(/^https?:\/\//, "", host); sub(/:[0-9]+$/, "", host)
      if (host ~ /^\[/) { sub(/^\[/, "", host); sub(/\]$/, "", host); if (valid6(host)) exit 0; exit 1 }
      c=split(host, labels, ".")
      for (i=1; i<=c; i++) if (labels[i] !~ /^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$/) exit 1
      exit 0 }
'; then
    die '--server must be an http:// or https:// origin with no path, query, fragment or userinfo.'
fi
case $server in
    http://*) [ -z "$ca_source" ] || die '--ca-file requires an HTTPS server.' ;;
esac
authority=${server#*://}
case $authority in
    *]:*) port=${authority##*:} ;;
    \[*\]) port= ;;
    *:*) port=${authority##*:} ;;
    *) port= ;;
esac
if [ -n "$port" ]; then
    if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then die 'Invalid server port.'; fi
fi
if [ -n "$version" ]; then
    printf '%s\n' "$version" | awk '
        NR != 1 || $0 !~ /^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$/ { bad=1 }
        END { exit bad }
    ' || die 'Invalid version (use e.g. 0.1.0, without v).'
fi
[ -n "$local_binary" ] || [ -n "$version" ] || die '--version or --binary is required.'
case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die 'Unsupported architecture (supported: x86_64, aarch64, arm64).' ;;
esac
if [ -n "$local_binary" ]; then
    if [ ! -f "$local_binary" ] || [ ! -s "$local_binary" ]; then die '--binary must name a nonempty regular file.'; fi
fi
[ -z "$ca_source" ] || [ -f "$ca_source" ] || die '--ca-file must name a regular PEM file.'
[ -z "$token_source" ] || [ -f "$token_source" ] || die '--token-file must name a regular file.'
if command -v runuser >/dev/null 2>&1; then
    use_runuser=1
elif command -v su >/dev/null 2>&1; then
    use_runuser=0
else
    die 'Install runuser or su to enroll as the non-root agent user.'
fi

temp_parent=$root/tmp
safe_path "$temp_parent"
mkdir -p "$temp_parent"
tmp=$(mktemp -d "$temp_parent/orbitbeacon-install.XXXXXXXX")
chmod 0700 "$tmp"

if [ -n "$local_binary" ]; then
    say 'Using the supplied local binary (verify its provenance before installing).'
    cp -- "$local_binary" "$tmp/agent"
else
    say "Downloading release v$version for $arch..."
    agent_asset=orbitbeacon-agent-linux-$arch
    release=https://github.com/mr-monkeyray/orbit-beacon/releases/download/v$version
    for asset in "$agent_asset" SHA256SUMS; do
        curl --fail --silent --show-error --location --proto '=https' \
            --proto-redir '=https' --tlsv1.2 --output "$tmp/$asset" "$release/$asset"
    done
    # Select exactly one entry, never trust unrelated manifest paths/options.
    awk -v name="$agent_asset" '
        ($2 == name || $2 == "*" name) {
            if (NF != 2 || length($1) != 64 || $1 !~ /^[[:xdigit:]]+$/) bad=1
            print $1 "  " name
            count++
        }
        END { if (count != 1 || bad) exit 1 }
    ' "$tmp/SHA256SUMS" > "$tmp/verify.sha256" || die 'Missing or malformed release checksum.'
    say 'Verifying release checksum...'
    (cd "$tmp" && sha256sum -c verify.sha256) || die 'Checksum verification failed; nothing installed.'
    mv -- "$tmp/$agent_asset" "$tmp/agent"
    [ -s "$tmp/agent" ] || die 'Release binary is empty or missing.'
fi

say 'Checking the dedicated system account...'
group_name_exists() {
    if command -v getent >/dev/null 2>&1; then
        getent group "$account" >/dev/null 2>&1
    else
        awk -F: -v name="$account" '$1 == name { found=1 } END { exit !found }' /etc/group
    fi
}
if ! id -u "$account" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        if group_name_exists; then
            useradd --system --gid "$account" --home-dir "$state" \
                --no-create-home --shell /usr/sbin/nologin "$account"
        else
            useradd --system --user-group --home-dir "$state" \
                --no-create-home --shell /usr/sbin/nologin "$account"
        fi
    elif command -v adduser >/dev/null 2>&1 && command -v addgroup >/dev/null 2>&1; then
        # BusyBox/Alpine equivalent; preserve a pre-existing system group.
        if ! group_name_exists; then addgroup -S "$account"; fi
        adduser -S -D -H -h "$state" -s /usr/sbin/nologin -G "$account" "$account"
    else
        die 'Install useradd, or BusyBox adduser/addgroup, and retry.'
    fi
fi
agent_uid=$(id -u "$account")
agent_gid=$(id -g "$account")
case $agent_uid:$agent_gid in *[!0-9:]*|:*|*:) die 'Cannot determine numeric agent UID/GID.' ;; esac
if [ "$agent_uid" -eq 0 ] || [ "$agent_gid" -eq 0 ]; then die 'The agent account must have a non-root UID and GID.'; fi
[ "$(id -gn "$account")" = "$account" ] || die 'The agent primary group must be orbitbeacon-agent (as required by the service unit).'

say 'Installing binary, private state directory and root-owned policy...'
mkdir -p "$bin_dir" "$config_dir" "$state"
chown root:root "$bin_dir" "$config_dir"
chmod 0755 "$bin_dir" "$config_dir"
chown "$agent_uid:$agent_gid" "$state"
chmod 0700 "$state"
# Rename in the destination filesystem also permits replacing a running binary.
binary_tmp=$(mktemp "$bin_dir/.orbitbeacon-agent.XXXXXXXX")
install -m 0755 "$tmp/agent" "$binary_tmp"
chown root:root "$binary_tmp"
mv -f -- "$binary_tmp" "$binary"
binary_tmp=
ca_file=
if [ -n "$ca_source" ]; then
    # Stage the CA in the root-owned config directory, including same-file reruns.
    policy_tmp=$(mktemp "$config_dir/.ca.XXXXXXXX")
    install -m 0644 "$ca_source" "$policy_tmp"
    chown root:root "$policy_tmp"
    mv -f -- "$policy_tmp" "$ca_copy"
    policy_tmp=
    ca_file=$ca_copy
fi
policy_tmp=$(mktemp "$config_dir/.install-policy.XXXXXXXX")
cat > "$policy_tmp" <<EOF
{
  "server_url": "$server",
  "ca_file": "$ca_file",
  "state_dir": "$state",
  "expected_uid": $agent_uid,
  "expected_gid": $agent_gid,
  "expected_account": "$account",
  "install_mode": "native"
}
EOF
chown root:root "$policy_tmp"
chmod 0644 "$policy_tmp"
mv -f -- "$policy_tmp" "$policy"
policy_tmp=
# Keep administrator edits on reinstall; the root-owned directory prevents the
# agent from replacing this file or planting a symlink during creation.
safe_path "$runtime_config"
if [ ! -e "$runtime_config" ]; then
    policy_tmp=$(mktemp "$config_dir/.agent.XXXXXXXX")
    printf '%s\n' '{}' > "$policy_tmp"
    chown "root:$agent_gid" "$policy_tmp"
    chmod 0640 "$policy_tmp"
    mv -fT -- "$policy_tmp" "$runtime_config"
    policy_tmp=
fi

# Read secrets using shell builtins/redirection only; never put one in argv.
if [ -n "$token_source" ]; then
    cat -- "$token_source" > "$tmp/token"
elif [ ! -t 0 ]; then
    cat > "$tmp/token"
elif [ ! -f "$state/identity.json" ]; then
    printf 'Enrollment token (hidden): ' >/dev/tty
    tty_state=$(stty -g </dev/tty)
    stty -echo </dev/tty
    token=
    IFS= read -r token </dev/tty || :
    stty "$tty_state" </dev/tty
    tty_state=
    printf '\n' >/dev/tty
    printf '%s\n' "$token" > "$tmp/token"
    unset token
fi
if [ -s "$tmp/token" ]; then
    # Do not replace a symlink planted in an existing state directory.
    safe_path "$token_file"
    chmod 0600 "$tmp/token"
    chown "$agent_uid:$agent_gid" "$tmp/token"
    # GNU/BusyBox mv -T treats the target as a file even if the agent races us
    # with a symlink to a directory. Never chown a path in agent-owned state as
    # root after moving the staged file into that directory.
    mv -fT -- "$tmp/token" "$token_file"
    say 'Enrolling as the non-root agent user...'
    # Quiet the child: even a failed/malicious endpoint must not echo a token.
    # Do not redirect diagnostics to a log that could retain the token either.
    if [ "$use_runuser" = 1 ]; then
        if runuser -u "$account" -- "$binary" enroll --install-policy "$policy" --config "$runtime_config" --token-file "$token_file" >/dev/null 2>&1; then
            enrolled=1
        else enrolled=0
        fi
    else
        # All interpolated paths are fixed, or guarded ASCII test-root paths.
        if su -s /bin/sh "$account" -c "exec '$binary' enroll --install-policy '$policy' --config '$runtime_config' --token-file '$token_file'" >/dev/null 2>&1; then
            enrolled=1
        else enrolled=0
        fi
    fi
    if [ "$enrolled" != 1 ]; then
        say "Enrollment failed. Keep $token_file; do not delete it before retrying."
        say "Retry as $account: $binary enroll --install-policy $policy --config $runtime_config --token-file $token_file"
        die 'Check the server URL, CA, network and token expiry. Service was not enabled.'
    fi
    say 'Enrollment succeeded (the agent removes the token on success).'
elif [ -f "$state/identity.json" ]; then
    say 'Existing identity found; keeping it and skipping enrollment.'
else
    die 'No enrollment token supplied. Use --token-file, stdin or the terminal prompt and retry.'
fi

say 'Installing the systemd service unit...'
mkdir -p "$unit_dir"
chown root:root "$unit_dir"
chmod 0755 "$unit_dir"
unit_tmp=$(mktemp "$unit_dir/.orbitbeacon-agent.XXXXXXXX")
# Keep this embedded unit exactly in sync with deploy/systemd/orbitbeacon-agent.service.
cat > "$unit_tmp" <<'UNIT'
[Unit]
Description=OrbitBeacon Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# These accounts must match expected_uid/expected_gid in the install policy.
User=orbitbeacon-agent
Group=orbitbeacon-agent
StateDirectory=orbitbeacon-agent
StateDirectoryMode=0700
ExecStart=/usr/local/bin/orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json --config /etc/orbitbeacon/agent.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10s
# A clone must be enrolled separately instead of reclaiming the same identity.
RestartPreventExitStatus=3
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
# Keep home mounts visible so their disk statistics describe the actual host.
ProtectHome=read-only
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native
SystemCallFilter=@system-service
# Interface lookups need netlink; host CPU statistics also need all of /proc.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
UMask=0077
MemoryMax=128M
TasksMax=64

[Install]
WantedBy=multi-user.target
UNIT
chown root:root "$unit_tmp"
chmod 0644 "$unit_tmp"
mv -f -- "$unit_tmp" "$unit"
unit_tmp=
if has_systemd; then
    systemctl daemon-reload
    systemctl enable --now orbitbeacon-agent
    # enable --now leaves an already-running service on the previous binary.
    systemctl restart orbitbeacon-agent
    say 'Installed and started. View logs: journalctl -u orbitbeacon-agent.service'
else
    say 'systemd is not running. Run in the foreground as the agent user:'
    if [ "$use_runuser" = 1 ]; then
        say "runuser -u $account -- $binary run --install-policy $policy --config $runtime_config"
    else
        say "su -s /bin/sh $account -c \"exec '$binary' run --install-policy '$policy' --config '$runtime_config'\""
    fi
fi
