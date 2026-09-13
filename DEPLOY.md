# 直接部署到 Linux 服务器

部署包包含 `bin/jobcompass-linux-amd64`、配置模板和 systemd / Caddy 文件，不需要在服务器安装 Go、Node 或数据库服务。

1. 解压后进入 `jobcompass` 目录，复制 `.env.example` 为 `.env`。
2. 编辑 `.env`，填入 `ANTHROPIC_API_KEY` 和账号实际可用的 `ANTHROPIC_MODEL`。如果使用兼容网关，同时修改 `ANTHROPIC_BASE_URL` 和实际处理方 `PROVIDER_NAME`。
3. 本机预览运行：

   ```bash
   ./bin/jobcompass-linux-amd64 --env-file .env
   ```

   默认只监听 `127.0.0.1:8080`。没有模型配置时展示固定诊断和求职准备示例，新的真实诊断和 AI 生成不可用；已有版本仍可手动编辑和导出。

4. 对外提供服务时，按 `README.md` 的 Linux 部署步骤建立独立用户并配置 Caddy。设置 `APP_ENV=production`、实际的 `PUBLIC_ORIGIN=https://你的域名`、`DATA_DIR=/opt/jobcompass/data` 和实际部署地区。
5. 用 `openssl rand -hex 32` 生成并安全保存 `DATA_ENCRYPTION_KEY`；填入配置。生产启动会检查 HTTPS 配置和加密密钥。
6. 将 `deploy/jobcompass.service` 安装到 systemd；修改 Caddy 的域名，验证配置后启动。检查 `/healthz` 并完成一份获授权样本的真实诊断。

若服务器是 ARM64，请从源码运行 `bash scripts/build.sh arm64`，不要使用本包的 amd64 程序。

材料、报告及其全部岗位版本、补充事实和面试记录默认随原报告保留 7 天，可随时删除。不要把 `.env`、数据库和密钥放在任何静态网站目录内。程序的网页资源已内嵌，无需配置数据库目录的文件访问。

## 从首版升级

先停止服务并按原有保留期限妥善保存数据目录和加密密钥的恢复副本，再替换二进制。沿用原 `.env`、`DATA_DIR` 和 `DATA_ENCRYPTION_KEY`；不要重新生成加密密钥。启动时自动升级到 schema version 4，新增求职准备、账务及新用户赠送记录，已有诊断、会话、恢复码、订单和已购次数继续可用。

新增可选配置 `PREPARATION_ACTIONS_PER_IP_HOUR=60`、`PREPARATION_ACTIONS_PER_SESSION_DAY=120`；不填写也采用这些默认值。诊断和准备共用原有 worker 与队列容量。升级后用一个已有报告检查精修、复制岗位版本及面试回答的保存和恢复。

本版本新增个人收款码与人工核款，不依赖自动支付回调。首次升级保持购买关闭；需要使用管理页时，在原 `.env` 增加独立的 `BILLING_ADMIN_KEY`，再从 `/#admin` 上传真实收款码、填写套餐价格和核款联系方式。配置完整后手动开启。不要替换原数据加密密钥。

从已启用收款的版本升级时，保留原管理密钥、收款码、套餐和购买开关。升级后新注册账号自动获得诊断、精修、岗位适配和文字面试各一次免费体验，旧账号不自动补发。验证新账号的四项余额、首次使用后的扣次和再次登录后的余额；最终生成失败应退回对应次数。

订单、账号和次数账本独立于报告保留期保存。v4 账本允许赠送记录不关联付款订单，旧程序不支持这一规则；回滚时不能直接让旧程序读取已发放体验次数的数据库，也不能用升级前的副本覆盖上线后新增的账号、订单或消费。应在恢复对外服务前核对迁移结果，成功后清理临时材料备份。详细配置与退款操作见 `docs/billing.md`，工程验收见 `docs/verification.md`。
