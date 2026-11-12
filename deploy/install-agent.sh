#!/bin/sh
# POSIX sh 安装器。禁止用 sh -x 运行，避免泄露密钥。
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
用法：sh install-agent.sh --server URL (--version VERSION | --binary FILE) [选项]
      sh install-agent.sh --uninstall [--yes]
      sh install-agent.sh --help

安装与信任：
  --server URL              Server 的 HTTP(S) 根地址，不含路径或凭据
  --version VERSION         固定发布版本，如 0.1.0（不带 v）
  --binary FILE             使用本地二进制，跳过下载；此时版本可省略
  --ca-file FILE            Server 的私有 CA PEM；HTTP 不可使用
  --key KEY                 会出现在进程列表和 shell 历史中，适用于一次性密钥
  --token-file FILE         从文件读取密钥，也支持 stdin 或终端隐藏输入
  --update-base-url URL     HTTPS 发布镜像根地址；重装默认保留，'' 关闭远程升级

账户与服务：
  --uid N / --gid N         专用账户的非零 UID/GID；默认自动分配，已有账户须匹配
  --service MODE            auto（默认）、systemd、openrc 或 none（前台）
  --no-start                安装服务但不启用、不启动、不重启已有服务

下载与维护：
  --download-timeout N      每个文件下载超时秒数（默认 300，1–3600）
  --download-retries N      下载失败重试次数（默认 3，0–10）
  --uninstall              确认后删除本地安装及全部身份状态；保留账户
  --yes                    仅用于卸载时跳过确认
  -h, --help               显示帮助，无需 root

Linux：识别 CentOS/RHEL、Debian、Ubuntu、Alpine 及其衍生系统。
架构：x86_64/amd64、aarch64/arm64；不提供 32 位发布包。
安装需 root；Agent 始终以 orbitbeacon-agent 非 root 运行。
固定安装到 /opt/orbit-beacon，重装保留身份和 agent.yaml。
例：sh install-agent.sh --server https://beacon.example.com --version 0.1.0
    sh install-agent.sh --server https://beacon.example.com --binary ./agent --service none
USAGE
}

server='' version='' ca_source='' token_source='' key='' local_binary=''
default_update_base=https://github.com/mr-monkeyray/orbit-beacon/releases/download
update_base_url=$default_update_base
update_base_set=0
uninstall=0 yes=0
service=auto no_start=0 requested_uid='' requested_gid=''
download_timeout=300 download_retries=3
while [ "$#" -gt 0 ]; do
    case $1 in
        --server|--version|--ca-file|--token-file|--key|--binary|--update-base-url|--uid|--gid|--service|--download-timeout|--download-retries)
            [ "$#" -ge 2 ] || die "缺少 $1 的参数值"
            # base64url 密钥可能以 -- 开头，按字面量接收。
            case $2 in --*) [ "$1" = --key ] || die "缺少 $1 的参数值" ;; esac
            [ -n "$2" ] || [ "$1" = --update-base-url ] || die "缺少 $1 的参数值"
            case $1 in
                --update-base-url) update_base_url=$2; update_base_set=1 ;;
                --server) server=$2 ;;
                --version) version=$2 ;;
                --ca-file) ca_source=$2 ;;
                --token-file) token_source=$2 ;;
                --key) key=$2 ;;
                --binary) local_binary=$2 ;;
                --uid) requested_uid=$2 ;;
                --gid) requested_gid=$2 ;;
                --service) service=$2 ;;
                --download-timeout) download_timeout=$2 ;;
                --download-retries) download_retries=$2 ;;
            esac
            shift 2 ;;
        --uninstall) uninstall=1; shift ;;
        --yes) yes=1; shift ;;
        --no-start) no_start=1; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die '未知选项。请使用 --help。' ;;
    esac
done

[ -z "$key" ] || [ -z "$token_source" ] || die '--key 与 --token-file 不能同时使用。'

case $service in auto|systemd|openrc|none) ;; *) die '--service 必须是 auto、systemd、openrc 或 none。' ;; esac
check_number() {
    case $2 in ''|*[!0-9]*|0[0-9]*) die "$1 必须是范围 $3–$4 内的十进制整数。" ;; esac
    if [ "${#2}" -gt 10 ] || [ "$2" -lt "$3" ] || [ "$2" -gt "$4" ]; then
        die "$1 超出允许范围 $3–$4。"
    fi
}
[ -z "$requested_uid" ] || check_number --uid "$requested_uid" 1 2147483647
[ -z "$requested_gid" ] || check_number --gid "$requested_gid" 1 2147483647
check_number --download-timeout "$download_timeout" 1 3600
check_number --download-retries "$download_retries" 0 10

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

case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die '不支持的架构；仅提供 64 位 x86_64/amd64 与 aarch64/arm64 发布包。' ;;
esac
# os-release 是系统提供的 shell 格式文件，不读取用户指定文件。
ID=unknown ID_LIKE='' VERSION_ID=''
if [ -r "$root/etc/os-release" ]; then
    # shellcheck disable=SC1091
    . "$root/etc/os-release"
fi
case " $ID $ID_LIKE " in
    *' alpine '*) os_family=alpine; packages='apk add curl ca-certificates coreutils openrc' ;;
    *' ubuntu '*|*' debian '*) os_family=debian; packages='apt-get install curl ca-certificates coreutils passwd util-linux procps' ;;
    *' centos '*|*' rhel '*|*' fedora '*) os_family=rhel; packages='dnf install curl ca-certificates coreutils shadow-utils util-linux procps-ng（旧版用 yum）' ;;
    *) os_family=linux; packages='请用系统包管理器安装缺失工具' ;;
esac
say "检测到系统：$ID ${VERSION_ID:-未知版本}（$os_family），架构：$arch"

account=orbitbeacon-agent
install_root=$root/opt/orbit-beacon
bin_dir=$install_root/bin
libexec=$install_root/libexec
binary=$bin_dir/orbitbeacon-agent
config_dir=$install_root/etc
policy=$config_dir/install-policy.yaml
runtime_config=$config_dir/agent.yaml
ca_copy=$config_dir/ca.pem
state=$install_root/state
token_file=$state/enrollment.token
unit_dir=$root/etc/systemd/system
unit=$unit_dir/orbitbeacon-agent.service
openrc_unit=$root/etc/init.d/orbitbeacon-agent
tmp='' policy_tmp='' binary_tmp='' token_tmp='' unit_tmp='' tty_state=''
cleanup() {
    if [ -n "$tty_state" ]; then stty "$tty_state" </dev/tty || :; fi
    [ -z "$policy_tmp" ] || rm -f -- "$policy_tmp"
    [ -z "$binary_tmp" ] || rm -f -- "$binary_tmp"
    [ -z "$token_tmp" ] || rm -f -- "$token_tmp"
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
# bin/state 的目录入口由 root 所有的安装根保护；叶子由不跟随链接的操作处理。
for path in "$bin_dir" "$libexec" "$state" "$policy" "$runtime_config" "$ca_copy" "$unit" "$openrc_unit"; do
    safe_path "$path"
done
has_systemd() {
    command -v systemctl >/dev/null 2>&1 && [ -d "$root/run/systemd/system" ]
}
has_openrc() {
    command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1 && command -v start-stop-daemon >/dev/null 2>&1
}
has_identity() {
    # stat 默认检查链接本身；root 只探测类型，不读取 Agent 可控制的身份文件。
    [ "$(stat -c '%F' -- "$state/identity.json" 2>/dev/null)" = 'regular file' ]
}

if [ "$uninstall" = 1 ]; then
    if [ "$update_base_set" != 0 ] || [ -n "$server$version$ca_source$token_source$key$local_binary$requested_uid$requested_gid" ] || [ "$service" != auto ] || [ "$no_start" != 0 ] || [ "$download_timeout:$download_retries" != 300:3 ]; then die '不要将安装选项与 --uninstall 混用。'; fi
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
    if has_openrc && [ -f "$openrc_unit" ]; then
        rc-service --ifstarted orbitbeacon-agent stop
        rc-update del orbitbeacon-agent default >/dev/null 2>&1 || :
    fi
    rm -f -- "$unit" "$openrc_unit"
    # 不加末尾斜杠；rm 不跟随树内的符号链接，也不运行 Agent 二进制。
    rm -rf -- "$install_root"
    if has_systemd; then systemctl daemon-reload; fi
    say '已卸载。系统账户保留；如尚未吊销服务端凭据请自行处理。'
    exit 0
fi
[ "$yes" = 0 ] || die '--yes 仅与 --uninstall 搭配使用。'
if [ "$service" = auto ]; then
    if has_systemd; then service=systemd
    elif has_openrc; then service=openrc
    else service=none
    fi
fi
case $service in
    systemd)
        command -v systemctl >/dev/null 2>&1 || die '请安装 systemd 或选择 --service none。'
        [ -x /bin/kill ] || die "systemd reload 需要 /bin/kill。依赖安装参考：$packages" ;;
    openrc) has_openrc || die '请安装 OpenRC（含 start-stop-daemon）或选择 --service none。' ;;
esac
if { [ "$service" != systemd ] && [ -e "$unit" ]; } || { [ "$service" != openrc ] && [ -e "$openrc_unit" ]; }; then
    die '检测到另一种已安装的服务。请先停止并禁用旧服务、删除旧服务文件（保留 /opt/orbit-beacon），再切换 --service。'
fi
install_mode=native
[ "$service" != none ] || install_mode=foreground
for tool in awk stat mkdir mktemp chmod chown cp install cat mv rm sed; do
    command -v "$tool" >/dev/null 2>&1 || die "缺少 $tool。依赖安装参考：$packages"
done
if [ -z "$local_binary" ]; then
    for tool in curl sha256sum; do
        command -v "$tool" >/dev/null 2>&1 || die "缺少 $tool。依赖安装参考：$packages"
    done
fi
# JSON 本身也是 YAML；只复制配置内容，不移动或重建身份目录。
for name in install-policy agent; do
    legacy=$config_dir/$name.json
    current=$config_dir/$name.yaml
    if { [ -e "$legacy" ] || [ -L "$legacy" ]; } && [ ! -e "$current" ]; then
        die "发现旧配置 $legacy。请先备份，再将该普通文件的原有内容安全复制到 $current（JSON 是 YAML 子集，可直接加载），设置 root 属主和 0640 权限；保留 $state/identity.json，然后重试。"
    fi
done
[ ! -e "$runtime_config" ] || [ -f "$runtime_config" ] || die '本地 Agent 配置必须是普通文件。'
[ ! -e "$policy" ] || [ -f "$policy" ] || die '安装策略必须是普通文件。'
[ -n "$server" ] || die '必须指定 --server。'
# 保守的 ASCII Origin 语法同时保证 YAML 字符串和 shell 安全。
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
# shortcut: 只读取安装器生成的扁平 YAML；需自动保留任意策略写法时改由二进制解析。
if [ "$update_base_set" = 0 ] && [ -f "$policy" ]; then
    update_base_url=$(awk '
        /^[[:space:]]*(#.*)?$/ { next }
        /^[a-z_]+: ("[^"\\]*"|[0-9]+)$/ {
            key=$0; sub(/:.*/, "", key)
            if (key !~ /^(server_url|update_base_url|ca_file|state_dir|expected_uid|expected_gid|expected_account|install_mode)$/ || seen[key]++) bad=1
            count++
            if (key == "update_base_url") {
                if ($0 !~ /^update_base_url: "[^"\\]*"$/) bad=1
                source=$0; sub(/^update_base_url: "/, "", source); sub(/"$/, "", source)
            }
            next
        }
        { bad=1 }
        END { if (bad || count != 8 || !seen["update_base_url"]) exit 1; print source }
    ' "$policy") || die "无法安全读取 $policy 的发布源；请显式指定 --update-base-url HTTPS地址，或恢复安装器生成的 YAML 格式（update_base_url: \"\" 表示关闭远程升级）。"
fi
# HTTPS 根地址同时用于安装下载和后续远程升级；限制字符以安全写入 YAML。
update_base_url=$(printf '%s' "$update_base_url" | sed 's:/*$::')
if [ -n "$update_base_url" ]; then
    # 用变量传正则：BusyBox awk 不接受字面量里字符类中的 /。
    printf '%s\n' "$update_base_url" | awk -v re='^https://([A-Za-z0-9.-]+|[[][0-9A-Fa-f:]+])(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$' '
        NR != 1 || $0 !~ re { bad=1 }
        END { exit bad }
    ' || die '--update-base-url 必须是 HTTPS 地址，且不含用户信息、查询或片段。'
fi
if [ -n "$version" ]; then
    printf '%s\n' "$version" | awk '
        NR != 1 || $0 !~ /^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$/ { bad=1 }
        END { exit bad }
    ' || die '无效版本号（示例：0.1.0，不带 v）。'
fi
[ -n "$local_binary" ] || [ -n "$version" ] || die '必须指定 --version 或 --binary。'
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
    release=${update_base_url:-$default_update_base}/v$version
    for asset in "$agent_asset" SHA256SUMS; do
        curl --fail --silent --show-error --location --proto '=https' \
            --proto-redir '=https' --max-redirs 5 --tlsv1.2 --connect-timeout 15 \
            --max-time "$download_timeout" --retry "$download_retries" --retry-max-time "$download_timeout" \
            --output "$tmp/$asset" "$release/$asset"
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
if id -u "$account" >/dev/null 2>&1; then
    [ -z "$requested_uid" ] || [ "$(id -u "$account")" = "$requested_uid" ] || die '已有 Agent 账户 UID 与 --uid 不符；不会迁移或修改已有账户。'
    [ -z "$requested_gid" ] || [ "$(id -g "$account")" = "$requested_gid" ] || die '已有 Agent 账户 GID 与 --gid 不符；不会迁移或修改已有账户。'
fi
if [ -n "$requested_gid" ] && group_name_exists; then
    if command -v getent >/dev/null 2>&1; then
        existing_gid=$(getent group "$account" | cut -d: -f3)
    else
        existing_gid=$(awk -F: -v name="$account" '$1 == name { print $3 }' /etc/group)
    fi
    [ "$existing_gid" = "$requested_gid" ] || die '已有 Agent 组 GID 与 --gid 不符。'
fi
if ! id -u "$account" >/dev/null 2>&1; then
    nologin=/sbin/nologin
    [ ! -x /usr/sbin/nologin ] || nologin=/usr/sbin/nologin
    if command -v useradd >/dev/null 2>&1; then
        if ! group_name_exists; then
            command -v groupadd >/dev/null 2>&1 || die "缺少 groupadd。依赖安装参考：$packages"
            set -- --system
            [ -z "$requested_gid" ] || set -- "$@" --gid "$requested_gid"
            groupadd "$@" "$account"
        fi
        set -- --system --gid "$account" --home-dir "$state" --no-create-home --shell "$nologin"
        [ -z "$requested_uid" ] || set -- "$@" --uid "$requested_uid"
        useradd "$@" "$account"
    elif command -v adduser >/dev/null 2>&1 && command -v addgroup >/dev/null 2>&1; then
        # BusyBox/Alpine 等效操作；保留已有的系统组。
        if ! group_name_exists; then
            set -- -S
            [ -z "$requested_gid" ] || set -- "$@" -g "$requested_gid"
            addgroup "$@" "$account"
        fi
        set -- -S -D -H -h "$state" -s "$nologin" -G "$account"
        [ -z "$requested_uid" ] || set -- "$@" -u "$requested_uid"
        adduser "$@" "$account"
    else
        die '须安装 useradd 或 BusyBox adduser/addgroup 后重试。'
    fi
fi
agent_uid=$(id -u "$account")
agent_gid=$(id -g "$account")
case $agent_uid:$agent_gid in *[!0-9:]*|:*|*:) die '无法确定 Agent 的数字 UID/GID。' ;; esac
if [ "$agent_uid" -eq 0 ] || [ "$agent_gid" -eq 0 ]; then die 'Agent 账户的 UID 和 GID 不得为 root。'; fi
[ -z "$requested_uid" ] || [ "$agent_uid" = "$requested_uid" ] || die '账户 UID 不匹配。'
[ -z "$requested_gid" ] || [ "$agent_gid" = "$requested_gid" ] || die '账户 GID 不匹配。'
[ "$(id -gn "$account")" = "$account" ] || die 'Agent 主组必须是 orbitbeacon-agent（systemd 服务单元要求）。'

say '正在安装二进制、私有状态目录和归 root 所有的策略...'
# 先固定 root 所有的父目录，Agent 无法替换 bin/state/etc 的目录入口。
mkdir -p "$install_root"
chown -h "root:$agent_gid" "$install_root"
chmod 0750 "$install_root"
mkdir -p "$bin_dir" "$config_dir" "$state" "$libexec"
chown -h root:root "$libexec"
chmod 0755 "$libexec"
chown -h "root:$agent_gid" "$config_dir"
chmod 0750 "$config_dir"
chown -h "$agent_uid:$agent_gid" "$bin_dir" "$state"
chmod 0750 "$bin_dir"
chmod 0700 "$state"
# 暂存于同一文件系统内、Agent 不可写的父目录，避免 bin 内临时文件被调包。
# chown 后不再读写暂存文件；-T 原子替换叶子链接，绝不跟随目标目录链接。
binary_tmp=$(mktemp "$install_root/.orbitbeacon-agent.XXXXXXXX")
install -m 0750 "$tmp/agent" "$binary_tmp"
chown -h "$agent_uid:$agent_gid" "$binary_tmp"
mv -fT -- "$binary_tmp" "$binary"
binary_tmp=
ca_file=
if [ -n "$ca_source" ]; then
    # 将 CA 暂存到 root 所有的配置目录，重复安装同一文件也适用。
    policy_tmp=$(mktemp "$config_dir/.ca.XXXXXXXX")
    install -m 0640 "$ca_source" "$policy_tmp"
    chown -h "root:$agent_gid" "$policy_tmp"
    mv -fT -- "$policy_tmp" "$ca_copy"
    policy_tmp=
    ca_file=$ca_copy
fi
# 内嵌脚本与 deploy/orbitbeacon-agent-rollback 保持一致。
policy_tmp=$(mktemp "$libexec/.rollback.XXXXXXXX")
cat > "$policy_tmp" <<'ROLLBACK'
#!/bin/sh
# 以 Agent 用户运行；绝不执行 bin 中的文件。
# 唯一参数用于测试根目录，systemd 不传参数。
root=${1:-/opt/orbit-beacon}
marker=$root/state/upgrade-pending
[ -f "$marker" ] || exit 0
# 第四次启动仍未确认时回滚；异常内容按超限处理。
case $(cat "$marker" 2>/dev/null) in
    0) count=1 ;;
    1) count=2 ;;
    2) count=3 ;;
    *) count=4 ;;
esac
tmp=$(mktemp "$root/state/.upgrade-pending.XXXXXXXX") || exit 0
if ! printf '%s' "$count" > "$tmp" || ! mv -f "$tmp" "$marker"; then
    rm -f "$tmp"
    exit 0
fi
[ "$count" = 4 ] && [ -f "$root/bin/orbitbeacon-agent.prev" ] || exit 0
if mv -f "$root/bin/orbitbeacon-agent.prev" "$root/bin/orbitbeacon-agent"; then
    mv -f "$marker" "$root/state/upgrade-rolled-back" || :
fi
exit 0
ROLLBACK
chown -h root:root "$policy_tmp"
chmod 0755 "$policy_tmp"
mv -fT -- "$policy_tmp" "$libexec/orbitbeacon-agent-rollback"
policy_tmp=
policy_tmp=$(mktemp "$config_dir/.install-policy.XXXXXXXX")
cat > "$policy_tmp" <<EOF
# 修改后重启 Agent；归 root 所有，Agent 组只读（0640）。
# Server 根地址，不含路径、查询或凭据。
server_url: "$server"
# 可信 HTTPS 发布根地址；空为关闭远程升级。
update_base_url: "$update_base_url"
# CA PEM 路径；空用系统证书，HTTP 时必须留空。
ca_file: "$ca_file"
# 身份与运行状态目录；变更须停服并迁移原目录。
state_dir: "$state"
# 必须匹配实际非 root UID/GID；安装器自动填写。
expected_uid: $agent_uid
expected_gid: $agent_gid
# 运行账户名；空则只校验 UID/GID。
expected_account: "$account"
# native：系统服务；docker：容器；foreground：前台。此项不安装服务。
install_mode: "$install_mode"
EOF
chown -h "root:$agent_gid" "$policy_tmp"
chmod 0640 "$policy_tmp"
mv -fT -- "$policy_tmp" "$policy"
policy_tmp=
# 重装时保留管理员修改；root 所有的目录阻止 Agent 替换此文件或植入符号链接。
safe_path "$runtime_config"
if [ ! -e "$runtime_config" ]; then
    policy_tmp=$(mktemp "$config_dir/.agent.XXXXXXXX")
    cat > "$policy_tmp" <<'RUNTIME'
# 修改后通过服务 reload 或 SIGHUP 重新加载；重装保留本文件。
# 采集与上报间隔（秒，5–60）。
report_interval_seconds: 10
# 磁盘容量采集间隔（秒，60–3600）。
disk_interval_seconds: 60
# [] 使用默认路由网卡；最多 8 项，例如 ["eth0"]。
interfaces: []
# [] 使用根路径；最多 8 个绝对路径，例如 ["/", "/srv/data"]。
disk_paths: []
# [] 不探测；最多 5 个 TCP、HTTP(S)、DNS 或 ICMP 目标。
probe_targets: []
# HTTP(S) 预期状态码（100–599）；键须在 probe_targets 中。
probe_expected_status: {}
# [] 禁止全部远程指令；upgrade 还需安装策略配置可信发布源。
remote_commands: ["upgrade", "restart", "stop"]
# 可选：disk、network、traffic、probes、disk_io、pressure、temperature。
# 关闭 network 同时关闭 traffic；关闭 disk 同时关闭 disk_io。
disabled_features: []
RUNTIME
    mv -fT -- "$policy_tmp" "$runtime_config"
    policy_tmp=
fi
# 重装也固定属主和权限，保留管理员修改的内容。
chown -h "root:$agent_gid" "$runtime_config"
chmod 0640 "$runtime_config"

# printf 为 shell 内建；密钥只写入私有临时文件，不传给子进程参数。
if [ -n "$key" ]; then
    printf '%s\n' "$key" > "$tmp/token"
    unset key
elif [ -n "$token_source" ]; then
    cat -- "$token_source" > "$tmp/token"
elif [ ! -t 0 ]; then
    cat > "$tmp/token"
fi
# 空的非终端 stdin 也可回退到控制终端；已有身份时不提示。
if [ ! -s "$tmp/token" ] && [ -z "$token_source" ] && ! has_identity; then
    if tty_state=$(stty -g </dev/tty 2>/dev/null) 2>/dev/null; then
        stty -echo </dev/tty
        printf '密钥（隐藏输入）：' >/dev/tty
        token=
        IFS= read -r token </dev/tty || :
        stty "$tty_state" </dev/tty
        tty_state=
        printf '\n' >/dev/tty
        [ -z "$token" ] || printf '%s\n' "$token" > "$tmp/token"
        unset token
    fi
fi
if [ -s "$tmp/token" ]; then
    # 在同一文件系统的 root 所有目录内准备密钥，避免跨文件系统时 mv 退化为复制。
    token_tmp=$(mktemp "$install_root/.enrollment.XXXXXXXX")
    install -m 0600 "$tmp/token" "$token_tmp"
    chown -h "$agent_uid:$agent_gid" "$token_tmp"
    # GNU/BusyBox mv -T 将目标视为文件，即使 Agent 抢先创建了目录符号链接。
    # 移入暂存文件后，绝不以 root 对 Agent 状态目录中的路径执行属主变更。
    mv -fT -- "$token_tmp" "$token_file"
    token_tmp=
    say '正在以非 root Agent 用户注册...'
    # 静默子进程：即使端点故障或恶意也不得回显密钥。
    # 也不将诊断输出重定向到可能留存密钥的日志。
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
        say "注册失败。请保留 $token_file；服务未启用。"
        say "尚未提交且密钥仍有效时，以 $account 身份重试：$binary enroll --install-policy $policy --config $runtime_config --token-file $token_file"
        say "可能已提交但响应丢失时，以 $account 身份确认已有凭据：$binary enroll --install-policy $policy --config $runtime_config"
        say '确认成功后，不再传入字符串或密钥文件，使用原安装参数加 < /dev/null 重新运行安装器以补装并启动服务。恢复不会删除残留密钥。'
        die '请检查服务器 URL、CA、网络；密钥只能在 30 分钟内首次成功激活，重复使用返回 403。'
    fi
    say '注册成功（Agent 在成功后自行删除密钥）。'
elif has_identity; then
    say '已有身份；保留并跳过注册。'
else
    die '未提供密钥；请使用 --key 或终端提示。'
fi

if [ "$service" = systemd ]; then
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
ExecStartPre=-/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback
ExecStart=/opt/orbit-beacon/bin/orbitbeacon-agent run --install-policy /opt/orbit-beacon/etc/install-policy.yaml --config /opt/orbit-beacon/etc/agent.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10s
# 克隆主机须单独注册，不得复用同一身份。
RestartPreventExitStatus=3
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ReadWritePaths=/opt/orbit-beacon/bin /opt/orbit-beacon/state
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
    if [ "$no_start" = 0 ]; then
        systemctl enable orbitbeacon-agent
        systemctl restart orbitbeacon-agent
        say '已安装并启动。查看日志：journalctl -u orbitbeacon-agent.service'
    else
        say '已安装，未改变服务运行/启用状态。启动：systemctl enable --now orbitbeacon-agent'
    fi
else
    say 'systemd 未运行；启动后执行：systemctl enable --now orbitbeacon-agent'
fi
elif [ "$service" = openrc ]; then
    say '正在安装 OpenRC 服务...'
    policy_tmp=$(mktemp "$libexec/.supervise.XXXXXXXX")
    cat > "$policy_tmp" <<'SUPERVISOR'
#!/bin/sh
# 以 Agent 用户运行；保留 systemd 的 0/3 停止、其他退出重启语义。
set -u
root=${1:-/opt/orbit-beacon}
child= stopping=0
trap 'stopping=1; [ -z "$child" ] || kill -TERM "$child" 2>/dev/null || :' TERM INT
trap '[ -z "$child" ] || kill -HUP "$child" 2>/dev/null || :' HUP
while [ "$stopping" = 0 ]; do
    "$root/libexec/orbitbeacon-agent-rollback" "$root"
    "$root/bin/orbitbeacon-agent" run --install-policy "$root/etc/install-policy.yaml" --config "$root/etc/agent.yaml" &
    child=$!
    while :; do
        wait "$child"
        code=$?
        kill -0 "$child" 2>/dev/null || break
    done
    child=
    [ "$stopping" = 0 ] || break
    case $code in 0|3) exit "$code" ;; esac
    sleep 10 &
    child=$!
    wait "$child" || :
    child=
done
exit 0
SUPERVISOR
    chown root:root "$policy_tmp"
    chmod 0755 "$policy_tmp"
    mv -fT -- "$policy_tmp" "$libexec/orbitbeacon-agent-supervise"
    policy_tmp=
    mkdir -p "$root/etc/init.d"
    unit_tmp=$(mktemp "$root/etc/init.d/.orbitbeacon-agent.XXXXXXXX")
    cat > "$unit_tmp" <<'OPENRC'
#!/sbin/openrc-run
description="OrbitBeacon Agent"
command="/opt/orbit-beacon/libexec/orbitbeacon-agent-supervise"
command_user="orbitbeacon-agent:orbitbeacon-agent"
command_background=true
pidfile="/run/orbitbeacon-agent.pid"
output_logger="logger -t orbitbeacon-agent"
error_logger="logger -t orbitbeacon-agent"
umask=0077
retry="TERM/30/KILL/5"
extra_started_commands="reload"
description_reload="重读 Agent 本地配置"
depend() {
    need net
    after firewall
}
reload() {
    ebegin "Reloading OrbitBeacon Agent"
    start-stop-daemon --signal HUP --pidfile "$pidfile"
    eend $?
}
OPENRC
    chown root:root "$unit_tmp"
    chmod 0755 "$unit_tmp"
    mv -fT -- "$unit_tmp" "$openrc_unit"
    unit_tmp=
    if [ "$no_start" = 0 ]; then
        rc-update add orbitbeacon-agent default
        rc-service orbitbeacon-agent restart
        say '已安装并启动。日志见系统 syslog（标签 orbitbeacon-agent）。'
    else
        say '已安装，未改变服务运行/启用状态。启动：rc-update add orbitbeacon-agent default && rc-service orbitbeacon-agent start'
    fi
elif [ "$service" = none ]; then
    say '未安装服务。以 Agent 用户前台执行：'
    if [ "$use_runuser" = 1 ]; then
        say "runuser -u $account -- $binary run --install-policy $policy --config $runtime_config"
    else
        say "su -s /bin/sh $account -c \"exec '$binary' run --install-policy '$policy' --config '$runtime_config'\""
    fi
fi
