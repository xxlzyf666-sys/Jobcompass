// Deterministic local QA responses; never imported by the application.
export function preparationFixture(input) {
  const quote = text => text.split('\n').find(line => line.includes('Redis') && line.length >= 8) || text.split('\n').find(line => line.trim().length >= 8) || text.slice(0,120);
  const makeReview = () => ({ summary:'固定测试复盘：本轮用于验证逐题反馈与训练计划的展示，不评估真实面试水平。', strengths:['回答中说明了自己的工作范围。'], gaps:['需要更具体地解释技术选择和异常处理。'], practice_plan:['用背景、职责、方案和结果四步重讲项目。','选择一个失败场景，说明排查路径和方案取舍。'] });
  if (input.kind === 'questions') return { questions:[
    { quote:quote(input.version.text), question:'这段经历中，你具体负责哪些环节？哪些工作由同事完成？', why:'确认个人职责边界，避免夸大贡献。' },
    { quote:quote(input.version.text), question:'实现过程中遇到过什么具体问题，你如何选择和验证方案？', why:'补充技术取舍和可核实的处理过程。' },
    { quote:quote(input.version.text), question:'你有哪些实际结果可以说明？没有统计数据也可以如实说明。', why:'把能确认的事实写清楚，不估算业绩。' }
  ]};
  if (input.kind === 'rewrite' || input.kind === 'tailor') {
    const before = quote(input.version.text);
    const fact = input.version.facts?.[0];
    return { edits:[{ before, after:input.kind === 'rewrite' && fact ? `${before}\n${fact}` : `相关项目实践：${before}`, reason:'固定测试建议：根据已确认材料补充职责，并突出与岗位相关的实践。', evidence:[{source:'resume',quote:before}, ...(input.kind === 'rewrite' && fact ? [{source:'fact',quote:fact}] : [])] }], notes:['这是本地固定响应，只验证交互和引用核对，不代表真实模型输出质量。'] };
  }
  const i = input.interview;
  if (input.kind === 'interview_review') return { review:makeReview() };
  const prompts = ['请介绍这段项目经历：你具体负责哪些环节，为什么选择这些技术？','如果方案遇到并发更新或故障，你会怎样定位和处理？','你如何验证方案有效，哪些结论有实际依据？','如果重新设计，你会怎样权衡复杂度和维护成本？','你与同事如何拆分职责，怎样推进有分歧的技术选择？','你如何向新同事解释项目中的关键约束？','系统增长后，你会先观察什么，再决定是否调整设计？','请总结一次失败或不足，以及下一次会怎样改进。'];
  const n = i.turns.length;
  const question = { question:n ? `针对你刚才提到的“${i.turns.at(-1).answer.slice(0,45)}”：${prompts[n]}` : prompts[0], focus:'项目职责与设计取舍', resume_quote:quote(i.resume) };
  if (input.kind === 'interview_start') return { question };
  const feedback = { answer_quote:i.turns.at(-1).answer.slice(0,500), assessment:'固定测试反馈：回答已经提供了过程描述，还可以把个人职责与团队工作区分得更明确。', improvement:'结合你能确认的实际经历，说明当时的约束、备选方案以及验证方法。', outline:['先说明背景和自己承担的工作。','再解释技术选择、异常场景和可核实的结果。'] };
  return { feedback, ...(n < i.rounds ? {question} : {review:makeReview()}) };
}
