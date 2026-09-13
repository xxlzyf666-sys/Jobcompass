// Local integration-test fixture. This is not an AI model and is never used by default.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { preparationFixture } from './preparation-fixture.mjs';
if (!process.argv.includes('--test-only')) throw new Error('Run only for tests, with --test-only.');
const fixture = JSON.parse(await readFile(new URL('../internal/app/example.json', import.meta.url), 'utf8'));
const port = Number(process.env.JOBCOMPASS_MOCK_PORT || 9091);
const escapeHTML = (value) => value.replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const findQuote = (source, quote) => {
  const pattern = [...quote.replace(/\s/g, '')].map(c => c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('\\s*');
  return source.match(new RegExp(pattern, 'u'))?.[0] || quote;
};
const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url === '/fixture/resume') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>虚构 PDF 测试简历</title><style>body{font:13px/1.8 "Noto Sans CJK SC",sans-serif;margin:32px}pre{font:inherit;white-space:pre-wrap}@page{size:A4;margin:15mm}</style><h1>虚构 PDF 测试简历</h1><pre>${escapeHTML(fixture.input.resume)}</pre></html>`);
    return;
  }
  if (req.method !== 'POST' || req.url !== '/v1/messages') { res.writeHead(404); res.end(); return; }
  try {
    let raw = '';
    for await (const chunk of req) { raw += chunk; if (raw.length > 768000) throw new Error('Request too large'); }
    const request = JSON.parse(raw);
    const input = JSON.parse(request.messages[0].content);
    if (request.model !== 'local-fixture-not-ai' || !['deliver_diagnosis','deliver_preparation'].includes(request.tool_choice?.name)) throw new Error('Invalid test protocol');
    if (request.tool_choice.name === 'deliver_preparation') {
      const output = preparationFixture(input);
      await new Promise(resolve => setTimeout(resolve, 600));
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ model:'local-fixture-not-ai', stop_reason:'tool_use', usage:{input_tokens:0,output_tokens:0}, content:[{type:'tool_use',name:'deliver_preparation',input:output}] }));
      return;
    }
    const report = structuredClone(fixture.report);
    report.title = '本地联调样例 · Go 后端开发对照';
    report.summary = '本报告由本地固定测试响应生成，用于验证网站流程，不是真实 AI 诊断。' + report.summary;
    for (const item of report.requirements) {
      item.jd_quote = findQuote(input.jd, item.jd_quote);
      if (item.resume_quote) item.resume_quote = findQuote(input.resume, item.resume_quote);
    }
    const output = { title: report.title, summary: report.summary, requirements: report.requirements, actions: report.actions };
    await new Promise(resolve => setTimeout(resolve, 1600));
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ model: 'local-fixture-not-ai', stop_reason: 'tool_use', usage: { input_tokens: 0, output_tokens: 0 }, content: [{ type: 'tool_use', name: 'deliver_diagnosis', input: output }] }));
  } catch {
    res.writeHead(400, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ error: 'Local test fixture rejected the request.' }));
  }
});
server.listen(port, '127.0.0.1', () => process.stdout.write(`LOCAL TEST FIXTURE ONLY: http://127.0.0.1:${port}\n`));
