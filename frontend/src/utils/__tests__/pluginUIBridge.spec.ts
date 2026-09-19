import { describe, expect, it } from "vitest";

import {
  pluginBridgeExpectsResponse,
  pluginUIFrameReferrerPolicy,
} from "../pluginUIBridge";

describe("plugin UI bridge compatibility", () => {
  it("exposes only the parent origin so cross-origin plugin UIs can address the host", () => {
    expect(pluginUIFrameReferrerPolicy).toBe("origin");
  });

  it("tracks every bridge request that requires a result", () => {
    expect(pluginBridgeExpectsResponse("config.load")).toBe(true);
    expect(pluginBridgeExpectsResponse("config.save")).toBe(true);
    expect(pluginBridgeExpectsResponse("config.test")).toBe(true);
    expect(pluginBridgeExpectsResponse("plugin.status")).toBe(true);
    expect(pluginBridgeExpectsResponse("sub2api.plugin.ready")).toBe(false);
    expect(pluginBridgeExpectsResponse("ui.resize")).toBe(false);
  });
});
