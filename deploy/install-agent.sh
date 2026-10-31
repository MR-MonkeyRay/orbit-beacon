#!/bin/sh
# POSIX sh 安装器。禁止用 sh -x 运行，避免泄露注册票据。
set -eu
# 即使调用方误开 xtrace，也先关闭追踪以保护下方的敏感信息，之后不再启用。
set +x
umask 077
LC_ALL=C
export LC_ALL

say() { printf '%s\n' "[orbitbeacon] $*"; }
die() { printf '%s\n' "[orbitbeacon] ERROR: $*" >&2; exit 1; }
usage() {
    cat <<'USAGE'
用法：sh install-agent.sh --server http[s]://HOST[:PORT] --version VERSION
      [--ca-file FILE] [--token-file FILE] [--binary FILE]
      sh install-agent.sh --uninstall [--yes]
以 root 运行。--binary 替代下载（此时 --version 可选）。
通过文件、标准输入或终端的隐藏输入提示提供注册票据。
不接受通过命令行参数传入票据。保留已有身份。
USAGE
}

server='' version='' ca_source='' token_source='' local_binary=''
uninstall=0 yes=0
while [ "$#" -gt 0 ]; do
    case $1 in
        --server|--version|--ca-file|--token-file|--binary)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "缺少 $1 的参数值"; fi
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
        *) die '未知选项。请使用 --help；票据只能通过文件、标准输入或终端隐藏输入提示提供。' ;;
    esac
done

# 仅测试钩子：两个启用变量和哨兵文件缺一不可。正常安装始终使用真实绝对路径，测试框架提供命令桩。
root=
if [ "${ORBITBEACON_INSTALL_TEST:-}" = 1 ]; then
    root=${ORBITBEACON_INSTALL_ROOT:-}
    case $root in /tmp/*) ;; *) die '测试根目录必须是 /tmp 下的私有目录。' ;; esac
    case $root in *[!A-Za-z0-9_./-]*) die '不安全的测试根目录。' ;; esac
    if [ ! -d "$root" ] || [ ! -f "$root/.orbitbeacon-install-test-root" ]; then die '缺少测试根目录哨兵文件。'; fi
    [ "$(cd "$root" && pwd -P)" = "$root" ] || die '测试根目录必须是规范路径且不含符号链接。'
elif [ -n "${ORBITBEACON_INSTALL_ROOT:-}" ]; then
    die '根前缀仅受保护的测试框架支持。'
fi
[ "$(id -u)" = 0 ] || die '须以 root 运行此安装器。'
[ "$(uname -s)" = Linux ] || die '仅支持 Linux。'

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

# 拒绝目标路径及其祖先目录中的符号链接，卸载时同样适用。
safe_path() {
    checked=$1
    while [ "$checked" != / ]; do
        [ ! -L "$checked" ] || die "拒绝符号链接：$checked"
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
    [ -z "$server$version$ca_source$token_source$local_binary" ] || die '不要将安装选项与 --uninstall 混用。'
    say '请先在控制台吊销该节点凭据；本地删除不会吊销。'
    say '卸载将删除服务、二进制、策略、本地配置、CA 副本及全部 Agent 状态。请先停止前台 Agent。'
    if [ "$yes" != 1 ]; then
        printf '输入 yes 永久移除此安装：' >&2
        answer=
        read -r answer || :
        [ "$answer" = yes ] || die '卸载已取消；未删除任何文件。'
    fi
    if has_systemd; then
        # 上次卸载不完整时 unit 可能仍处于加载/启用状态。
        # unit 不存在无影响；继续删除本地文件。
        systemctl disable --now orbitbeacon-agent >/dev/null 2>&1 || :
    fi
    rm -f -- "$unit" "$binary" "$policy" "$runtime_config" "$ca_copy"
    rm -rf -- "$state"
    if has_systemd; then systemctl daemon-reload; fi
    say '已卸载。系统账户保留；如尚未吊销服务端凭据请自行处理。'
    exit 0
fi
[ "$yes" = 0 ] || die '--yes 仅与 --uninstall 搭配使用。'
[ ! -e "$runtime_config" ] || [ -f "$runtime_config" ] || die '本地 Agent 配置必须是普通文件。'
[ -n "$server" ] || die '必须指定 --server。'
# 保守的 ASCII Origin 语法同时保证 JSON 和 shell 安全。
# 允许单个末尾斜杠并规范化去除。禁止用户信息、路径、查询参数、
# 片段、空白、引号、反斜杠、IPv6 区域标识或百分号编码主机。
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
    die '--server 必须是 http:// 或 https:// Origin，不含路径、查询参数、片段或用户信息。'
fi
case $server in
    http://*) [ -z "$ca_source" ] || die '--ca-file 仅适用于 HTTPS 服务器。' ;;
esac
authority=${server#*://}
case $authority in
    *]:*) port=${authority##*:} ;;
    \[*\]) port= ;;
    *:*) port=${authority##*:} ;;
    *) port= ;;
esac
if [ -n "$port" ]; then
    if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then die '无效的服务器端口。'; fi
fi
if [ -n "$version" ]; then
    printf '%s\n' "$version" | awk '
        NR != 1 || $0 !~ /^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$/ { bad=1 }
        END { exit bad }
    ' || die '无效版本号（示例：0.1.0，不带 v）。'
fi
[ -n "$local_binary" ] || [ -n "$version" ] || die '必须指定 --version 或 --binary。'
case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die '不支持的架构（支持：x86_64、aarch64、arm64）。' ;;
esac
if [ -n "$local_binary" ]; then
    if [ ! -f "$local_binary" ] || [ ! -s "$local_binary" ]; then die '--binary 必须指定非空普通文件。'; fi
fi
[ -z "$ca_source" ] || [ -f "$ca_source" ] || die '--ca-file 必须指定 PEM 普通文件。'
[ -z "$token_source" ] || [ -f "$token_source" ] || die '--token-file 必须指定普通文件。'
if command -v runuser >/dev/null 2>&1; then
    use_runuser=1
elif command -v su >/dev/null 2>&1; then
    use_runuser=0
else
    die '请安装 runuser 或 su，以便使用非 root 的 Agent 用户注册。'
fi

temp_parent=$root/tmp
safe_path "$temp_parent"
mkdir -p "$temp_parent"
tmp=$(mktemp -d "$temp_parent/orbitbeacon-install.XXXXXXXX")
chmod 0700 "$tmp"

if [ -n "$local_binary" ]; then
    say '使用提供的本地二进制（安装前请自行验证来源）。'
    cp -- "$local_binary" "$tmp/agent"
else
    say "正在下载 v$version 的 $arch 发布包..."
    agent_asset=orbitbeacon-agent-linux-$arch
    release=https://github.com/mr-monkeyray/orbit-beacon/releases/download/v$version
    for asset in "$agent_asset" SHA256SUMS; do
        curl --fail --silent --show-error --location --proto '=https' \
            --proto-redir '=https' --tlsv1.2 --output "$tmp/$asset" "$release/$asset"
    done
    # 仅选取一条记录，不信任无关的清单路径或选项。
    awk -v name="$agent_asset" '
        ($2 == name || $2 == "*" name) {
            if (NF != 2 || length($1) != 64 || $1 !~ /^[[:xdigit:]]+$/) bad=1
            print $1 "  " name
            count++
        }
        END { if (count != 1 || bad) exit 1 }
    ' "$tmp/SHA256SUMS" > "$tmp/verify.sha256" || die '缺少或格式错误的发布校验和。'
    say '正在校验发布包...'
    (cd "$tmp" && sha256sum -c verify.sha256) || die '校验和验证失败；未安装任何文件。'
    mv -- "$tmp/$agent_asset" "$tmp/agent"
    [ -s "$tmp/agent" ] || die '发布二进制为空或不存在。'
fi

say '正在检查专用系统账户...'
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
        # BusyBox/Alpine 等效操作；保留已有的系统组。
        if ! group_name_exists; then addgroup -S "$account"; fi
        adduser -S -D -H -h "$state" -s /usr/sbin/nologin -G "$account" "$account"
    else
        die '须安装 useradd 或 BusyBox adduser/addgroup 后重试。'
    fi
fi
agent_uid=$(id -u "$account")
agent_gid=$(id -g "$account")
case $agent_uid:$agent_gid in *[!0-9:]*|:*|*:) die '无法确定 Agent 的数字 UID/GID。' ;; esac
if [ "$agent_uid" -eq 0 ] || [ "$agent_gid" -eq 0 ]; then die 'Agent 账户的 UID 和 GID 不得为 root。'; fi
[ "$(id -gn "$account")" = "$account" ] || die 'Agent 主组必须是 orbitbeacon-agent（systemd 服务单元要求）。'

say '正在安装二进制、私有状态目录和归 root 所有的策略...'
mkdir -p "$bin_dir" "$config_dir" "$state"
chown root:root "$bin_dir" "$config_dir"
chmod 0755 "$bin_dir" "$config_dir"
chown "$agent_uid:$agent_gid" "$state"
chmod 0700 "$state"
# 在目标文件系统内重命名，也允许替换正在运行的二进制。
binary_tmp=$(mktemp "$bin_dir/.orbitbeacon-agent.XXXXXXXX")
install -m 0755 "$tmp/agent" "$binary_tmp"
chown root:root "$binary_tmp"
mv -f -- "$binary_tmp" "$binary"
binary_tmp=
ca_file=
if [ -n "$ca_source" ]; then
    # 将 CA 暂存到 root 所有的配置目录，重复安装同一文件也适用。
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
# 重装时保留管理员修改；root 所有的目录阻止 Agent 替换此文件或植入符号链接。
safe_path "$runtime_config"
if [ ! -e "$runtime_config" ]; then
    policy_tmp=$(mktemp "$config_dir/.agent.XXXXXXXX")
    printf '%s\n' '{}' > "$policy_tmp"
    chown "root:$agent_gid" "$policy_tmp"
    chmod 0640 "$policy_tmp"
    mv -fT -- "$policy_tmp" "$runtime_config"
    policy_tmp=
fi

# 仅通过标准输入、文件或终端读取票据；绝不将其放入命令行参数。
if [ -n "$token_source" ]; then
    cat -- "$token_source" > "$tmp/token"
elif [ ! -t 0 ]; then
    cat > "$tmp/token"
elif [ ! -f "$state/identity.json" ]; then
    printf '注册票据（隐藏输入）：' >/dev/tty
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
    # 不替换已有状态目录中被植入的符号链接。
    safe_path "$token_file"
    chmod 0600 "$tmp/token"
    chown "$agent_uid:$agent_gid" "$tmp/token"
    # GNU/BusyBox mv -T 将目标视为文件，即使 Agent 抢先创建了目录符号链接。
    # 移入暂存文件后，绝不以 root 对 Agent 状态目录中的路径执行属主变更。
    mv -fT -- "$tmp/token" "$token_file"
    say '正在以非 root Agent 用户注册...'
    # 静默子进程：即使端点故障或恶意也不得回显票据。
    # 也不将诊断输出重定向到可能留存票据的日志。
    if [ "$use_runuser" = 1 ]; then
        if runuser -u "$account" -- "$binary" enroll --install-policy "$policy" --config "$runtime_config" --token-file "$token_file" >/dev/null 2>&1; then
            enrolled=1
        else enrolled=0
        fi
    else
        # 所有插值路径均为固定值或受保护的 ASCII 测试根路径。
        if su -s /bin/sh "$account" -c "exec '$binary' enroll --install-policy '$policy' --config '$runtime_config' --token-file '$token_file'" >/dev/null 2>&1; then
            enrolled=1
        else enrolled=0
        fi
    fi
    if [ "$enrolled" != 1 ]; then
        say "注册失败。请保留 $token_file；重试前不要删除。"
        say "以 $account 身份重试：$binary enroll --install-policy $policy --config $runtime_config --token-file $token_file"
        die '请检查服务器 URL、CA、网络和票据有效期。服务未启用。'
    fi
    say '注册成功（Agent 在成功后自行删除票据）。'
elif [ -f "$state/identity.json" ]; then
    say '已有身份；保留并跳过注册。'
else
    die '未提供注册票据。请通过 --token-file、stdin 或终端提示输入后重试。'
fi

say '正在安装 systemd 服务单元...'
mkdir -p "$unit_dir"
chown root:root "$unit_dir"
chmod 0755 "$unit_dir"
unit_tmp=$(mktemp "$unit_dir/.orbitbeacon-agent.XXXXXXXX")
# 此内嵌 unit 须与 deploy/systemd/orbitbeacon-agent.service 逐字节一致。
cat > "$unit_tmp" <<'UNIT'
[Unit]
Description=OrbitBeacon Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 该账户须与安装策略中的 expected_uid/expected_gid 一致。
User=orbitbeacon-agent
Group=orbitbeacon-agent
StateDirectory=orbitbeacon-agent
StateDirectoryMode=0700
ExecStart=/usr/local/bin/orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json --config /etc/orbitbeacon/agent.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10s
# 克隆主机须单独注册，不得复用同一身份。
RestartPreventExitStatus=3
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
# 保留 home 挂载可见，使磁盘统计反映真实主机。
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
# 网卡查询需要 netlink；主机 CPU 统计还需要完整的 /proc。
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
    # enable --now 不会替换已运行服务的旧二进制。
    systemctl restart orbitbeacon-agent
    say '已安装并启动。查看日志：journalctl -u orbitbeacon-agent.service'
else
    say 'systemd 未运行。以 Agent 用户前台执行：'
    if [ "$use_runuser" = 1 ]; then
        say "runuser -u $account -- $binary run --install-policy $policy --config $runtime_config"
    else
        say "su -s /bin/sh $account -c \"exec '$binary' run --install-policy '$policy' --config '$runtime_config'\""
    fi
fi
