#!/bin/sh
set -eu

state_dir="${ECS_UPDATE_DIR:-/update-state}"
project_dir="${ECS_PROJECT_DIR:-/workspace}"
compose_project="${ECS_COMPOSE_PROJECT_NAME:-ecs-controller}"
branch="${ECS_UPDATE_BRANCH:-main}"
request_file="$state_dir/request.json"
processing_file="$state_dir/request.processing.json"
status_file="$state_dir/status.json"
lock_dir="$state_dir/.lock"
update_request_id=""

mkdir -p "$state_dir"
rmdir "$lock_dir" 2>/dev/null || true

json_escape() {
    # Keep serialized values free of trailing characters so the web UI can
    # match request IDs and commits while polling the status file.
    printf '%s' "$1" | awk 'BEGIN { ORS="" } { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\r/, ""); printf "%s", $0 }'
}

write_status() {
    status="$1"
    phase="$2"
    message="$3"
    progress="${4:-0}"
    target="${5:-}"
    current="${6:-}"
    request_id="${update_request_id:-}"
    now="$(date -u +%s)"
    tmp="$status_file.tmp"
    cat >"$tmp" <<EOF
{"status":"$(json_escape "$status")","phase":"$(json_escape "$phase")","message":"$(json_escape "$message")","progress":$progress,"target_commit":"$(json_escape "$target")","current_commit":"$(json_escape "$current")","request_id":"$(json_escape "$request_id")","updated_at":$now}
EOF
    mv "$tmp" "$status_file"
}

read_field() {
    field="$1"
    file="$2"
    sed -n "s/.*\"$field\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

wait_for_controller() {
    health_target="$1"
    health_current="$2"
    health_attempt=0
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
        health_attempt=$((health_attempt + 1))
        health_progress=$((78 + health_attempt / 2))
        if [ "$health_progress" -gt 94 ]; then
            health_progress=94
        fi
        write_status running restarting "新版本服务启动中，正在等待健康检查（${health_attempt}/30）" "$health_progress" "$health_target" "$health_current"
        container_id="$(docker compose --project-name "$compose_project" -f "$project_dir/docker-compose.yml" ps -q ecs-controller 2>/dev/null | head -n 1)"
        health="$(docker inspect --format '{{.State.Health.Status}}' "$container_id" 2>/dev/null || true)"
        if [ "$health" = healthy ]; then
            return 0
        fi
        if [ "$health" = unhealthy ]; then
            return 1
        fi
        sleep 2
    done
    return 1
}

pull_controller_image() {
    pull_target="$1"
    pull_current="$2"
    pull_output="$state_dir/.pull-output-$$.log"
    pull_elapsed=0
    pull_progress=42

    docker compose --project-name "$compose_project" -f "$project_dir/docker-compose.yml" pull ecs-controller >"$pull_output" 2>&1 &
    pull_pid=$!
    while kill -0 "$pull_pid" 2>/dev/null; do
        write_status running pulling "正在拉取预构建 Docker 镜像（已等待 ${pull_elapsed} 秒）" "$pull_progress" "$pull_target" "$pull_current"
        sleep 2
        pull_elapsed=$((pull_elapsed + 2))
        if [ "$pull_progress" -lt 66 ]; then
            pull_progress=$((pull_progress + 1))
        fi
    done

    if wait "$pull_pid"; then
        rm -f "$pull_output"
        return 0
    fi
    rm -f "$pull_output"
    return 1
}

run_update() {
    target="$(read_field target_sha "$processing_file")"
    request_id="$(read_field request_id "$processing_file")"
    update_request_id="$request_id"
    current="$(git -C "$project_dir" rev-parse HEAD 2>/dev/null || true)"
    write_status queued queued "正在更新，准备校验目标版本" 8 "$target" "$current"

    if [ -z "$target" ]; then
        write_status error failed "更新请求缺少目标版本" 0 "$target" "$current"
        return
    fi
    if [ -n "$(git -C "$project_dir" status --porcelain 2>/dev/null || true)" ]; then
        write_status error failed "部署目录存在未提交修改，已停止更新以避免覆盖本地文件" 0 "$target" "$current"
        return
    fi

    write_status running fetching "正在从 GitHub 获取最新代码" 22 "$target" "$current"
    # The installer starts with a shallow clone. Unshallow it before merging so
    # Git can prove that the target commit is a fast-forward of the checkout.
    shallow="$(git -C "$project_dir" rev-parse --is-shallow-repository 2>/dev/null || printf '%s' false)"
    if [ "$shallow" = true ]; then
        fetch_args="--unshallow"
    else
        fetch_args=""
    fi
    if ! git -C "$project_dir" fetch $fetch_args origin "$branch"; then
        write_status error failed "GitHub 代码获取失败，请检查网络或仓库权限" 0 "$target" "$current"
        return
    fi
    remote="$(git -C "$project_dir" rev-parse "origin/$branch" 2>/dev/null || true)"
    if [ "$remote" != "$target" ]; then
        write_status error failed "GitHub 版本在检查后发生变化，请重新检查更新" 0 "$target" "$current"
        return
    fi

    write_status running pulling "正在准备拉取预构建 Docker 镜像" 42 "$target" "$current"
    export ECS_COMMIT="$target"
    export ECS_VERSION="$(printf '%s' "$target" | cut -c1-8)"
    export ECS_BUILD_DATE="$(git -C "$project_dir" show -s --format=%cI "$target" 2>/dev/null || printf '%s' unknown)"
    export ECS_IMAGE_TAG="sha-$target"
    if ! pull_controller_image "$target" "$current"; then
        write_status error failed "对应的预构建 Docker 镜像不存在或无法拉取，已停止更新" 0 "$target" "$current"
        return
    fi
    write_status running pulling "镜像已就绪，正在切换到目标版本" 70 "$target" "$current"
    if ! git -C "$project_dir" merge --ff-only "origin/$branch" >/dev/null 2>&1; then
        write_status error failed "本地代码无法快进到目标版本" 0 "$target" "$current"
        return
    fi
    write_status running restarting "正在重启 ECS 控制台" 78 "$target" "$target"
    if ! docker compose --project-name "$compose_project" -f "$project_dir/docker-compose.yml" up -d --no-build --force-recreate ecs-controller || ! wait_for_controller "$target" "$target"; then
        git -C "$project_dir" reset --hard "$current" >/dev/null 2>&1 || true
        export ECS_COMMIT="$current"
        export ECS_VERSION="$(printf '%s' "$current" | cut -c1-8)"
        export ECS_BUILD_DATE="$(git -C "$project_dir" show -s --format=%cI "$current" 2>/dev/null || printf '%s' unknown)"
        export ECS_IMAGE_TAG="sha-$current"
        docker compose --project-name "$compose_project" -f "$project_dir/docker-compose.yml" pull ecs-controller >/dev/null 2>&1 || true
        docker compose --project-name "$compose_project" -f "$project_dir/docker-compose.yml" up -d --no-build --force-recreate ecs-controller >/dev/null 2>&1 || true
        write_status error rolled_back "服务重启失败，已尝试恢复到更新前版本" 0 "$target" "$current"
        return
    fi
    write_status success completed "更新完成，当前已运行最新版本" 100 "$target" "$target"
    rm -f "$processing_file"
}

while :; do
    if [ ! -f "$request_file" ] && [ -f "$processing_file" ]; then
        mv "$processing_file" "$request_file"
    fi
    if [ -f "$request_file" ] && mkdir "$lock_dir" 2>/dev/null; then
        mv "$request_file" "$processing_file"
        run_update || write_status error failed "更新任务异常退出" 0
        rm -f "$processing_file"
        rmdir "$lock_dir" 2>/dev/null || true
    fi
    sleep 2
done
