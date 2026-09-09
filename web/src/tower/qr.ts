import QRCode from "qrcode";

/** Draws the join link as a QR code sized for a phone to scan off a laptop screen. */
export function renderQR(canvas: HTMLCanvasElement, text: string): Promise<void> {
  return QRCode.toCanvas(canvas, text, { errorCorrectionLevel: "M", margin: 2, width: 256 });
}
