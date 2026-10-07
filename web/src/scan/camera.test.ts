import { expect, test } from "vitest";
import { facingFromLabel, hasOtherSameFacing } from "./camera";

const cam = (deviceId: string, label: string) => ({ deviceId, label, kind: "videoinput", groupId: "" }) as MediaDeviceInfo;

test("facing comes from Chrome, Safari and Firefox labels", () => {
  expect(facingFromLabel("camera2 0, facing back")).toBe("environment");
  expect(facingFromLabel("camera2 1, facing front")).toBe("user");
  expect(facingFromLabel("Back Ultra Wide Camera")).toBe("environment");
  expect(facingFromLabel("Front Camera")).toBe("user");
  expect(facingFromLabel("Camera 0, Facing back, Orientation 90")).toBe("environment");
  expect(facingFromLabel("FaceTime HD Camera")).toBeUndefined();
});

test("another camera counts only when it faces the same way", () => {
  const back = cam("b0", "camera2 0, facing back");
  const front = cam("f1", "camera2 1, facing front");
  const back2 = cam("b2", "camera2 2, facing back");
  expect(hasOtherSameFacing([back, front], "b0")).toBe(false);
  expect(hasOtherSameFacing([back, front, back2], "b0")).toBe(true);
  expect(hasOtherSameFacing([back, front], "b0", "environment")).toBe(false);
  expect(hasOtherSameFacing([back], "b0")).toBe(false);
  // desktop webcams: facing unknown, any second camera counts
  expect(hasOtherSameFacing([cam("w1", "FaceTime HD Camera"), cam("w2", "USB Camera")], "w1")).toBe(true);
});
