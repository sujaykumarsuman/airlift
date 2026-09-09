// Shape Detection API (Chrome, Android WebView, Safari 17+ behind a flag).
// Not in lib.dom; declared here so the decoder can feature-detect it.
interface DetectedBarcode {
  rawValue: string;
  format: string;
  boundingBox: DOMRectReadOnly;
  cornerPoints: ReadonlyArray<{ x: number; y: number }>;
}

interface BarcodeDetectorOptions {
  formats?: string[];
}

interface BarcodeDetectorInstance {
  detect(source: ImageBitmapSource): Promise<DetectedBarcode[]>;
}

interface BarcodeDetectorCtor {
  new (options?: BarcodeDetectorOptions): BarcodeDetectorInstance;
  getSupportedFormats(): Promise<string[]>;
}
