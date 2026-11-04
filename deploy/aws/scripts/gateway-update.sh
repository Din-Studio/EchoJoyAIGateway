#!/usr/bin/env bash
# deploy.sh 的停机发布流程；不独立执行。CloudFormation 更新期间保持 desired=0、min=0。
# AWS RegisterScalableTarget 会把容量夹回 min/max，故仅 SuspendedState=true 不够。

# 所有参数都是非密钥元数据，保留失败现场供人工恢复。
gateway_capture() {
  local before=$1 values
  umask 077
  mkdir -p "$DEPLOY_DIR/.backup"
  GW_STATE=$(mktemp -d "$DEPLOY_DIR/.backup/gateway-update.XXXXXX")
  GW_ACTIVE=0
  GW_CLUSTER=""
  GW_SERVICE=""
  GW_DESIRED=0
  GW_MIN=2
  GW_MAX=24
  GW_IN=false
  GW_OUT=false
  GW_SCHEDULED=false
  printf '%s\n' "$GATEWAY_DEPLOY_TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$' \
    || die "GATEWAY_DEPLOY_TIMEOUT_SECONDS 必须是正整数"
  if [ "$before" != NONE ]; then
    GW_CLUSTER=$(stack_output "$STACK_GATEWAY" ClusterName)
    GW_SERVICE=$(stack_output "$STACK_GATEWAY" ServiceName)
    aws ecs describe-services --cluster "$GW_CLUSTER" --services "$GW_SERVICE" --output json > "$GW_STATE/service-before.json"
    aws application-autoscaling describe-scalable-targets --service-namespace ecs \
      --resource-ids "service/$GW_CLUSTER/$GW_SERVICE" --scalable-dimension ecs:service:DesiredCount \
      --output json > "$GW_STATE/scaling-before.json"
    values=$(python3 - "$GW_STATE" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
s = json.loads((p / 'service-before.json').read_text())
a = json.loads((p / 'scaling-before.json').read_text())['ScalableTargets']
if s.get('failures') or len(s['services']) != 1 or len(a) != 1:
    sys.exit('服务或扩缩目标不唯一')
s, a = s['services'][0], a[0]
if s['status'] != 'ACTIVE' or len(s['deployments']) != 1:
    sys.exit('服务不处于稳定状态')
if s['deployments'][0].get('rolloutState') != 'COMPLETED':
    sys.exit('现有部署尚未完成')
n = s['desiredCount']
if n <= 0 or s['runningCount'] != n or s['pendingCount'] != 0:
    sys.exit('服务已停机或不稳定；先按原现场记录恢复，不得覆盖原容量')
if not 0 <= a['MinCapacity'] <= n <= a['MaxCapacity']:
    sys.exit('当前任务数不在扩缩范围内')
flags = a.get('SuspendedState', {})
print(n, a['MinCapacity'], a['MaxCapacity'], *(str(flags.get(k, False)).lower() for k in
    ['DynamicScalingInSuspended', 'DynamicScalingOutSuspended', 'ScheduledScalingSuspended']))
PY
    )
    read -r GW_DESIRED GW_MIN GW_MAX GW_IN GW_OUT GW_SCHEDULED <<< "$values"
  fi
  log "发布前现场：${GW_STATE}（原任务数 ${GW_DESIRED}）"
}

gateway_suspend() {
  aws application-autoscaling register-scalable-target --service-namespace ecs \
    --resource-id "service/$GW_CLUSTER/$GW_SERVICE" --scalable-dimension ecs:service:DesiredCount \
    --min-capacity 0 --max-capacity "$GW_MAX" \
    --suspended-state 'DynamicScalingInSuspended=true,DynamicScalingOutSuspended=true,ScheduledScalingSuspended=true' >/dev/null
}

gateway_stop() {
  aws ecs update-service --cluster "$GW_CLUSTER" --service "$GW_SERVICE" --desired-count 0 \
    --deployment-configuration 'deploymentCircuitBreaker={enable=true,rollback=false}' >/dev/null
}

# 不能只等 service.runningCount=0：STOPPING/DEACTIVATING 任务的进程尚未退出。
# list-tasks 的两种 desired-status 都检查，避免遗漏缩容中以及最后一刻调度的任务。
gateway_tasks_stopped() {
  local desired arns arn status
  for desired in RUNNING STOPPED; do
    arns=$(aws ecs list-tasks --cluster "$GW_CLUSTER" --service-name "$GW_SERVICE" \
      --desired-status "$desired" --query taskArns --output text) || return 2
    for arn in $arns; do
      [ "$arn" != None ] || continue
      status=$(aws ecs describe-tasks --cluster "$GW_CLUSTER" --tasks "$arn" \
        --query 'tasks[0].lastStatus' --output text) || return 2
      [ "$status" = STOPPED ] || return 1
    done
  done
}

# 扩容不一定新建 deployment，旧的 COMPLETED 不能代替 ALB 健康检查。
# host 网络的目标是 EC2 实例；必须匹配本轮目标任务所在实例，而非只统计健康目标数量。
gateway_targets_healthy() {
  local expected=$1 arns arn task instance groups group count=0
  : > "$GW_STATE/target-instances.txt"
  arns=$(aws ecs list-tasks --cluster "$GW_CLUSTER" --service-name "$GW_SERVICE" \
    --desired-status RUNNING --query taskArns --output text) || return 2
  for arn in $arns; do
    [ "$arn" != None ] || continue
    task=$(aws ecs describe-tasks --cluster "$GW_CLUSTER" --tasks "$arn" \
      --query 'tasks[0].[taskDefinitionArn,lastStatus,containerInstanceArn]' --output json) || return 2
    instance=$(python3 -c '
import json, sys
v = json.load(sys.stdin)
if isinstance(v, list) and len(v) == 3 and v[0] == sys.argv[1] and v[1] == "RUNNING" and v[2]: print(v[2])
' "$expected" <<< "$task") || return 2
    [ -n "$instance" ] || return 1
    instance=$(aws ecs describe-container-instances --cluster "$GW_CLUSTER" --container-instances "$instance" \
      --query 'containerInstances[0].ec2InstanceId' --output text) || return 2
    [ -n "$instance" ] && [ "$instance" != None ] || return 2
    printf '%s\n' "$instance" >> "$GW_STATE/target-instances.txt"
    count=$((count + 1))
  done
  [ "$count" -eq "$GW_DESIRED" ] || return 1
  groups=$(python3 - "$GW_STATE/service-current.json" <<'PY'
import json, sys
balances = json.load(open(sys.argv[1]))['services'][0]['loadBalancers']
if len(balances) != 2 or any(b['containerName'] != 'gateway' or b['containerPort'] != 3001 for b in balances):
    sys.exit('服务必须包含 gateway:3001 的两个目标组')
groups = {b['targetGroupArn'] for b in balances}
if len(groups) != 2: sys.exit('目标组不唯一')
print(' '.join(sorted(groups)))
PY
  ) || return 2
  for group in $groups; do
    aws elbv2 describe-target-health --target-group-arn "$group" --output json > "$GW_STATE/target-health.json" || return 2
    if ! python3 - "$GW_STATE/target-instances.txt" "$GW_STATE/target-health.json" "$GW_DESIRED" <<'PY'
import json, pathlib, sys
instances = set(pathlib.Path(sys.argv[1]).read_text().splitlines())
targets = json.load(open(sys.argv[2]))['TargetHealthDescriptions']
healthy = {x['Target']['Id'] for x in targets if x['Target']['Port'] == 3001 and x['TargetHealth']['State'] == 'healthy'}
sys.exit(0 if len(instances) == int(sys.argv[3]) and instances <= healthy else 1)
PY
    then return 1; fi
  done
}

gateway_wait() {
  local mode=$1 expected=${2:-} deadline state rc
  deadline=$((SECONDS + GATEWAY_DEPLOY_TIMEOUT_SECONDS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    aws ecs describe-services --cluster "$GW_CLUSTER" --services "$GW_SERVICE" --output json > "$GW_STATE/service-current.json"
    state=$(python3 - "$GW_STATE/service-current.json" "$mode" "$GW_DESIRED" "$expected" <<'PY'
import json, sys
j = json.load(open(sys.argv[1]))
if j.get('failures') or len(j['services']) != 1:
    sys.exit('服务查询失败')
s = j['services'][0]
if s['status'] != 'ACTIVE':
    sys.exit('服务不是 ACTIVE')
if sys.argv[2] == 'stopped':
    ready = s['desiredCount'] == s['runningCount'] == s['pendingCount'] == 0
else:
    d = s['deployments']
    if any(x['taskDefinition'] == sys.argv[4] and x.get('rolloutState') == 'FAILED' for x in d):
        raise SystemExit('新任务部署失败；不得自动回退旧镜像')
    ready = (s['taskDefinition'] == sys.argv[4] and s['desiredCount'] == s['runningCount'] == int(sys.argv[3])
             and s['pendingCount'] == 0 and len(d) == 1 and d[0].get('rolloutState') == 'COMPLETED')
print('ready' if ready else 'waiting')
PY
    )
    if [ "$state" = ready ]; then
      if [ "$mode" = stopped ]; then
        if gateway_tasks_stopped; then return 0; else rc=$?; fi
      else
        if gateway_targets_healthy "$expected"; then return 0; else rc=$?; fi
      fi
      [ "$rc" -eq 1 ] || return "$rc"
    fi
    sleep 10
  done
  log "等待 $mode 超时（${GATEWAY_DEPLOY_TIMEOUT_SECONDS} 秒）"
  return 1
}

gateway_failed() {
  local status=$?
  trap - EXIT INT TERM
  if [ "$status" -ne 0 ] && [ "$GW_ACTIVE" -eq 1 ]; then
    # 尽力保持停机。AWS 权限/网络失败时必须人工检查，不能声称恢复成功。
    gateway_suspend || log "警告：暂停扩缩失败，必须人工检查"
    gateway_stop || log "警告：归零任务数失败，必须人工检查"
    log "发布失败；未恢复旧镜像或数据库。保留现场：$GW_STATE"
  fi
  exit "$status"
}

# 参数为原 cloudformation deploy 的参数；只追加发布窗口和原扩缩状态。
gateway_deploy_maintenance() {
  local before=$1 expected image values
  shift
  gateway_capture "$before"
  trap gateway_failed EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if [ "$before" != NONE ]; then
    GW_ACTIVE=1
    gateway_suspend
    gateway_stop
    gateway_wait stopped
    set -- "$@" "MinTasks=$GW_MIN" "MaxTasks=$GW_MAX" \
      "SuspendScaleIn=$GW_IN" "SuspendScaleOut=$GW_OUT" "SuspendScheduledScaling=$GW_SCHEDULED"
  fi
  # 显式 min=0/desired=0，防止更新 ScalableTarget 时把任务数夹回原下限。
  aws cloudformation deploy --stack-name "$STACK_GATEWAY" --template-file "$DEPLOY_DIR/gateway.yaml" \
    --parameter-overrides "$@" MaintenanceWindow=true --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset --disable-rollback
  GW_CLUSTER=$(stack_output "$STACK_GATEWAY" ClusterName)
  GW_SERVICE=$(stack_output "$STACK_GATEWAY" ServiceName)
  GW_ACTIVE=1
  if [ "$before" = NONE ]; then
    values=$(aws cloudformation describe-stacks --stack-name "$STACK_GATEWAY" \
      --query "Stacks[0].Parameters[?ParameterKey=='MinTasks' || ParameterKey=='MaxTasks'].[ParameterKey,ParameterValue]" --output json)
    values=$(python3 -c '
import json, sys
p = dict(json.load(sys.stdin)); n = int(p["MinTasks"]); m = int(p["MaxTasks"])
if not 0 < n <= m: sys.exit("MinTasks/MaxTasks 无效")
print(n, m)' <<< "$values")
    read -r GW_DESIRED GW_MAX <<< "$values"
    GW_MIN=$GW_DESIRED
  fi
  gateway_wait stopped
  expected=$(aws cloudformation describe-stack-resource --stack-name "$STACK_GATEWAY" --logical-resource-id TaskDefinition \
    --query StackResourceDetail.PhysicalResourceId --output text)
  image=$(aws ecs describe-task-definition --task-definition "$expected" \
    --query 'taskDefinition.containerDefinitions[?name==`gateway`].image | [0]' --output text)
  [ "$image" = "$GW_EXPECTED_IMAGE" ] || die "任务定义镜像与目标不一致，保持停机"
  printf '%s\n' "$expected" > "$GW_STATE/target-task-definition.txt"
  aws ecs update-service --cluster "$GW_CLUSTER" --service "$GW_SERVICE" \
    --task-definition "$expected" --desired-count "$GW_DESIRED" >/dev/null
  gateway_wait healthy "$expected"
  # 不改变镜像，只关闭窗口；省略 DesiredCount 会保留已恢复的当前任务数。
  aws cloudformation deploy --stack-name "$STACK_GATEWAY" --template-file "$DEPLOY_DIR/gateway.yaml" \
    --parameter-overrides "$@" MaintenanceWindow=false --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset --disable-rollback
  GW_ACTIVE=0
  trap - EXIT INT TERM
  log "停机发布成功，已恢复任务数和原扩缩状态；现场：$GW_STATE"
}
