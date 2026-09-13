# 岗位罗盘 JobCompass

面向后端开发求职者的岗位准备网站：从简历与 JD 对照，继续完成事实追问、简历精修、岗位版本与文字模拟面试。

已经实现：文字输入、浏览器内 PDF 提取与检查、私有异步诊断、引文校验、报告筛选、恢复码、Markdown / PDF 导出、删除及到期清理。

每份已完成的报告都可以进入“求职准备”：

- **简历精修**：生成 2—5 个针对原文的追问，保存本人确认的事实，再生成逐段改写。并排查看原文、建议和依据；逐条采纳或保留原文。补充事实可以更正和删除。
- **岗位版本**：从现有简历复制独立版本，填写新 JD，生成岗位修改建议，编辑并保存成稿。最多 8 个版本；可导出纯文字，或在独立打印页另存为 PDF。导出仅含已保存正文。
- **文字面试**：选择项目深挖、后端技术或协作表达，练习 3、5 或 8 题。根据实际回答继续追问，保存逐题反馈与总结，也可提前结束并复盘已答部分。支持复盘下载和针对薄弱点再练；最多保存 6 场练习。

新增资料继续使用原报告的访问权限、恢复码和到期时间。面试保存开始时的简历快照；后续编辑版本不会改变历史面试。当前没有图片 OCR、公开分享、支付或会员系统。

项目文档：[计划书](docs/plan.md) · [求职准备设计](docs/preparation.md) · [部署指南](DEPLOY.md) · [验收记录](docs/verification.md)。

## 本地运行

开发需要 Go 1.25 或更新的受支持版本。PDF.js 静态资源已随源码准备，常规 Go 构建无需 Node；只有更新 PDF.js 时需要 Node 22.13+。

```bash
cp .env.example .env
go run -buildvcs=false ./cmd/jobcompass --env-file .env
```

访问 `http://127.0.0.1:8080`。开发时请让访问地址与 `PUBLIC_ORIGIN` 一致。

未配置模型时，网站提供明确标注的固定报告、求职准备示例和本地 PDF 提取预览，新的真实诊断与 AI 生成不可用。已有私有版本仍可查看、手动保存和导出。示例不会冒充 AI 结果。

要启用真实诊断，在 `.env` 中配置：

```dotenv
ANTHROPIC_API_KEY=你的服务端密钥
ANTHROPIC_MODEL=你的账号实际可调用的模型标识
ANTHROPIC_BASE_URL=https://api.anthropic.com
PROVIDER_NAME=Anthropic Claude
```

重启服务后生效。端点需要支持 Anthropic Messages 协议和工具输出。诊断使用 `deliver_diagnosis`，求职准备使用 `deliver_preparation` 及按任务区分的 JSON Schema；服务器核验字段、枚举、引用、数字和关联关系。引用核验不能证明全部语义正确，采纳前仍需本人确认。模型标识不写死；真实接入需对实际账号与模型做一次联调。

不要将 API Key 放到网页或前端脚本中。配置兼容端点时，也要把 `PROVIDER_NAME` 改为实际处理方，并核实材料处理说明。

## 为 2 核 1GB 构建

在开发机完成构建，再上传二进制；服务器不用安装 Go、Node、数据库服务或 PDF 解析器。

```bash
bash scripts/build.sh amd64
# ARM 服务器改用：bash scripts/build.sh arm64
```

构建结果为 `bin/jobcompass-linux-amd64`（或 arm64）。所有网页、PDF.js 及规则文本都嵌入二进制。

```bash
./bin/jobcompass-linux-amd64 --env-file .env
```

默认监听 `127.0.0.1:8080`。诊断与求职准备共用 2 个 worker，并交替优先取任务；两个队列合计最多 20 个待处理任务。每份工作台同一时间只生成一个结果，并用版本号防止多窗口覆盖、重复回答和迟到结果写回。PDF 上限 5 MiB、6 页，原文件只在用户浏览器解析；简历 PDF 也由浏览器打印，服务器无需运行浏览器。

求职准备默认限每 IP 每小时 60 次 AI 操作、每会话每天 120 次，分别通过 `PREPARATION_ACTIONS_PER_IP_HOUR` 和 `PREPARATION_ACTIONS_PER_SESSION_DAY` 配置。它们与诊断提交次数分开计算，手动编辑及导出不消耗 AI 操作次数。部署文件对应用设置了 384 MiB 的内存上限与 192 MiB 的 Go 内存软目标；这不是实际用量保证，需结合负载观察。

## Linux + systemd + Caddy 部署

1. 将构建结果、`.env.example` 和 `deploy/` 传到服务器，准备自己的域名及 Caddy。将 DNS 指向服务器。
2. 创建独立运行用户及目录（在服务器执行）：

   ```bash
   sudo useradd --system --home /opt/jobcompass --shell /usr/sbin/nologin jobcompass
   sudo install -d -o jobcompass -g jobcompass -m 700 /opt/jobcompass/data
   sudo install -m 755 bin/jobcompass-linux-amd64 /opt/jobcompass/jobcompass
   sudo install -o root -g jobcompass -m 640 .env.example /opt/jobcompass/.env
   ```

3. 编辑 `/opt/jobcompass/.env`。设置 `APP_ENV=production`、`PUBLIC_ORIGIN=https://你的域名`、`DATA_DIR=/opt/jobcompass/data`、实际模型参数及实际 `DATA_REGION`。`LISTEN_ADDR` 保持 `127.0.0.1:8080`。
4. 用 `openssl rand -hex 32` 生成密钥，填入 `DATA_ENCRYPTION_KEY`，并单独安全保存。丢失密钥后无法解密原数据库；更换为错误密钥时服务会拒绝启动。
5. 修改 `deploy/Caddyfile` 的域名，安装服务配置：

   ```bash
   sudo install -m 644 deploy/jobcompass.service /etc/systemd/system/jobcompass.service
   sudo install -m 644 deploy/Caddyfile /etc/caddy/Caddyfile
   sudo systemctl daemon-reload
   sudo systemctl enable --now jobcompass
   sudo systemctl reload caddy
   ```

   上述 Caddy 命令适用于专门为本项目安装的实例；已有其他站点时把站点块合并进现有配置。发布前先运行 `caddy validate --config /etc/caddy/Caddyfile`。

6. 检查 `https://你的域名/healthz`，然后用一份获授权且已移除不必要个人信息的样本完成真实模型联调。日志可用 `journalctl -u jobcompass -n 50` 查看，不记录材料全文或密钥。

项目尚未替你操作远程服务器、配置真实域名或建立收款渠道。

## 数据处理

- 匿名 Cookie 只用于会话识别；数据库存储令牌摘要。真实接口有会话授权和 CSRF 校验。
- 报告 ID 不是访问凭据。恢复码单独生成，只显示一次；持有码的人可以查看和删除该报告，请勿公开。
- PDF 原文件不上传。简历文字和 JD 会发送给配置的模型处理方，告知文案应与实际端点一致。
- 提交文字、报告、补充事实、岗位版本、面试快照与回答使用 AES-256-GCM 加密。它们统一按原报告的到期时间清理，默认 7 天；新增版本或练习不会延长时间。`RETENTION_HOURS` 可缩短但不能超过 168 小时。
- 启动时及每 30 秒清理到期数据，所有读取也检查期限。删除报告会级联删除准备资料、任务及访问权，取消正在运行的请求；迟到结果不能重新创建记录。单个岗位版本或面试也可单独删除；删除版本保留已开始面试中的快照。
- 应用不写材料全文到日志，不开启长期材料备份。若自行添加备份，必须设置独立加密、访问控制与到期策略，恢复前清除过期数据。
- 开发模式的 `data/development.key` 只适合本地预览；生产必须设置独立的 `DATA_ENCRYPTION_KEY`。

网络接收方已经收到的数据受其服务政策约束；删除本网站记录不等于撤回已发送的请求。正式开放时，应按实际部署位置、运营主体和接收方核实对用户的说明。

## 验证

```bash
go test -buildvcs=false ./...
go vet -buildvcs=false ./...
```

17 组测试覆盖私有访问、恢复及撤销、引用来源、输入边界、同意与 CSRF、加密与到期、任务重试 / 租约恢复、并发重复提交、运行中删除、上游协议和错误处理，以及简历逐条采纳、岗位版本隔离、事实纠正、面试快照、连续回答、提前复盘、薄弱点重练和旧库升级。上游协议测试使用本地替身，不发出真实 API 请求。

浏览器联调可单独使用 `scripts/mock-claude.mjs --test-only`。它绑定回环地址、只返回固定虚构结果，页面中应把 `PROVIDER_NAME` 设置为“本地测试（模拟响应，不是真实 AI）”，模型使用 `local-fixture-not-ai`。该脚本仅验证应用流程，不验证模型质量。

真实模型质量验收表见 [docs/evaluation.md](docs/evaluation.md)。工程测试通过不代表真实模型或产品需求已验证。

## 更新 PDF.js

```bash
npm ci --ignore-scripts --omit=optional --no-audit --no-fund
npm run assets
```

`package-lock.json` 固定资源依赖版本。PDF.js 按需从本网站加载，不使用第三方 CDN。第三方许可保存在内嵌 `vendor/` 目录中。
