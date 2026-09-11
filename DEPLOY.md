# 直接部署到 Linux 服务器

部署包包含 `bin/jobcompass-linux-amd64`、配置模板和 systemd / Caddy 文件，不需要在服务器安装 Go、Node 或数据库服务。

1. 解压后进入 `jobcompass` 目录，复制 `.env.example` 为 `.env`。
2. 编辑 `.env`，填入 `ANTHROPIC_API_KEY` 和账号实际可用的 `ANTHROPIC_MODEL`。如果使用兼容网关，同时修改 `ANTHROPIC_BASE_URL` 和实际处理方 `PROVIDER_NAME`。
3. 本机预览运行：

   ```bash
   ./bin/jobcompass-linux-amd64 --env-file .env
   ```

   默认只监听 `127.0.0.1:8080`。没有模型配置时仅展示固定示例，真实诊断不可用。

4. 对外提供服务时，按 `README.md` 的 Linux 部署步骤建立独立用户并配置 Caddy。设置 `APP_ENV=production`、实际的 `PUBLIC_ORIGIN=https://你的域名`、`DATA_DIR=/opt/jobcompass/data` 和实际部署地区。
5. 用 `openssl rand -hex 32` 生成并安全保存 `DATA_ENCRYPTION_KEY`；填入配置。生产启动会检查 HTTPS 配置和加密密钥。
6. 将 `deploy/jobcompass.service` 安装到 systemd；修改 Caddy 的域名，验证配置后启动。检查 `/healthz` 并完成一份获授权样本的真实诊断。

若服务器是 ARM64，请从源码运行 `bash scripts/build.sh arm64`，不要使用本包的 amd64 程序。

材料和报告默认保留 7 天，可随时删除。不要把 `.env`、数据库和密钥放在任何静态网站目录内。程序的网页资源已内嵌，无需配置数据库目录的文件访问。

本版本没有支付、会员或公开报告分享；工程验收详情见 `docs/verification.md`。真实模型质量仍需配置后评估。
