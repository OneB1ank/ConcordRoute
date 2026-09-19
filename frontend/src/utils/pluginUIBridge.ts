// 插件配置页只向子页面暴露父页面 Origin，避免泄露管理路由、查询参数和 Fragment。
// 跨域部署时插件依靠该 Referrer 选择 postMessage 的目标 Origin；使用 no-referrer 会让消息静默丢失。
export const pluginUIFrameReferrerPolicy: ReferrerPolicy = "origin";

const pluginBridgeResponseTypes = new Set([
  "config.load",
  "config.save",
  "config.test",
  "plugin.status",
]);

// 需要响应的请求必须先登记 request_id，防止迟到结果跨 iframe 文档泄露。
export function pluginBridgeExpectsResponse(type: unknown): type is string {
  return typeof type === "string" && pluginBridgeResponseTypes.has(type);
}
