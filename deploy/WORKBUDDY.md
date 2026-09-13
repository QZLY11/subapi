# WorkBuddy (CodeBuddy CN) 集成部署说明

本分支在 sub2api 中原生集成了 WorkBuddy / CodeBuddy CN（`copilot.tencent.com`）平台。

- 平台标识：`workbuddy`
- 账户类型：`workbuddy_oauth`（OAuth 设备码登录，管理后台「一键登录」）
- 上游接口：`https://copilot.tencent.com/v2`
- 迁移：`backend/migrations/239_workbuddy_platform.sql`

## 一、镜像从哪来

服务器**不需要编译**。GitHub Actions 在云端构建镜像，服务器只做 `docker pull`。

工作流：`.github/workflows/workbuddy-image.yml`
- 触发：推送到 `workbuddy` 分支，或手动 `Actions -> WorkBuddy Image -> Run workflow`
- 产出 1（推荐）：GHCR 镜像 `ghcr.io/qzly11/subapi:workbuddy`
- 产出 2（兜底）：Release 附件 `sub2api-workbuddy-image.tar.gz`，无需任何凭据

## 二、在服务器上部署

```bash
# 1. 拉取镜像
docker pull ghcr.io/qzly11/subapi:workbuddy

# 2. 让 compose 使用该镜像（把已有部署的 image 行替换掉）
#    原：image: weishaw/sub2api:latest
#    新：image: ghcr.io/qzly11/subapi:workbuddy
sed -i 's|^    image: weishaw/sub2api:latest$|    image: ghcr.io/qzly11/subapi:workbuddy|' docker-compose.yml

# 3. 重建 sub2api 容器（postgres / redis 保持运行，数据不受影响）
docker compose up -d

# 4. 验证
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/health   # 期望 200
docker logs --tail 30 sub2api | grep -i "migrat\|error"
```

若 GHCR 拉取受限，改用 Release 附件：

```bash
curl -L -o /tmp/sub2api-wb.tar.gz \
  https://github.com/qzly11/subapi/releases/download/workbuddy-image/sub2api-workbuddy-image.tar.gz
docker load -i /tmp/sub2api-wb.tar.gz   # 载入后的镜像名仍为 ghcr.io/qzly11/subapi:workbuddy
```

## 三、验证 WorkBuddy 是否生效

```bash
# 迁移 239 应已应用（返回 239_workbuddy_platform.sql）
docker exec sub2api-postgres psql -U sub2api -d sub2api -tAc \
  "SELECT filename FROM schema_migrations ORDER BY filename DESC LIMIT 1"

# platform CHECK 约束应包含 workbuddy
docker exec sub2api-postgres psql -U sub2api -d sub2api -tAc \
  "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='user_platform_quotas_platform_check'"
```

管理后台操作：
1. 登录后台 -> 「账户」-> 新建账户
2. 平台选 **WorkBuddy**，类型选 **WorkBuddy OAuth**
3. 点「一键 OAuth 登录」，浏览器打开授权链接并完成腾讯账号登录
4. 回到后台点「轮询/完成」，账户创建成功后点「测试连接」验证
5. 在「分组」中把该账户放到分组，即可通过 `/v1/chat/completions` 调用

## 四、从源码构建（仅当需要自行构建时）

```bash
docker build -t sub2api-workbuddy:local -f Dockerfile .
```

或使用 `deploy/docker-compose.build.yml`（与官方 `docker-compose.local.yml` 的唯一区别是
`sub2api` 服务从本仓库源码构建，而非拉取不含 WorkBuddy 的官方预构建镜像）。

> 注意：构建需编译前端与 Go 后端，峰值内存约 3–4 GB。
> 内存 <1 GB 的服务器请勿本地构建，改用第一节的云端镜像。

## 五、回滚

```bash
sed -i 's|^    image: ghcr.io/qzly11/subapi:workbuddy$|    image: weishaw/sub2api:latest|' docker-compose.yml
docker compose up -d
```

迁移 239 只放宽了 CHECK 约束（新增允许值），不影响旧版本运行，无需回滚数据库。
