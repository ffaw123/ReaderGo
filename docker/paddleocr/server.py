import base64
import binascii
import os
import queue
import tempfile
import uuid
from typing import Any

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from paddleocr import PaddleOCR
from pydantic import BaseModel, Field


class OCRRequest(BaseModel):
    file: str
    file_type: int = Field(default=1, alias="fileType")
    text_rec_score_thresh: float = Field(default=0.0, alias="textRecScoreThresh")
    visualize: bool = False


# 每个并发请求使用独立推理实例，避免共享预测器的线程安全问题。
ocr_worker_count = max(1, int(os.getenv("OCR_ENGINE_WORKERS", "2")))
ocr_engines: queue.Queue[PaddleOCR] = queue.Queue(maxsize=ocr_worker_count)
for _ in range(ocr_worker_count):
    # mobile 模型显著降低 CPU 推理延迟；关闭 MKLDNN 规避 PaddlePaddle 3.3 PIR/oneDNN 异常。
    ocr_engines.put(
        PaddleOCR(
            device="cpu",
            enable_mkldnn=False,
            text_detection_model_name="PP-OCRv5_mobile_det",
            text_recognition_model_name="PP-OCRv5_mobile_rec",
            use_doc_orientation_classify=False,
            use_doc_unwarping=False,
            use_textline_orientation=False,
        )
    )
app = FastAPI(title="ReaderGo PaddleOCR Service")


@app.get("/health")
def health() -> dict[str, str]:
    # 模型在应用启动前完成初始化；能响应即代表 OCR 引擎已可接收请求。
    return {"status": "ready"}


@app.post("/ocr")
def recognize(request: OCRRequest):
    log_id = str(uuid.uuid4())
    if request.file_type != 1:
        return JSONResponse(
            status_code=400,
            content={"logId": log_id, "errorCode": 400, "errorMsg": "only image input is supported"},
        )

    try:
        image_bytes = base64.b64decode(request.file, validate=True)
    except (binascii.Error, ValueError):
        return JSONResponse(
            status_code=400,
            content={"logId": log_id, "errorCode": 400, "errorMsg": "file must be valid base64 image data"},
        )

    image_path = ""
    try:
        # PaddleOCR 对文件路径的输入兼容性最稳定；临时文件在本次请求结束后立即清理。
        with tempfile.NamedTemporaryFile(suffix=".jpg", delete=False) as image_file:
            image_file.write(image_bytes)
            image_path = image_file.name

        engine = ocr_engines.get()
        try:
            predictions = list(
                engine.predict(
                    image_path,
                    text_rec_score_thresh=request.text_rec_score_thresh,
                )
            )
        finally:
            ocr_engines.put(engine)

        texts, scores = extract_texts_and_scores(predictions, request.text_rec_score_thresh)
        return {
            "logId": log_id,
            "errorCode": 0,
            "errorMsg": "Success",
            "result": {
                "ocrResults": [
                    {
                        "prunedResult": {
                            "rec_texts": texts,
                            "rec_scores": scores,
                        }
                    }
                ]
            },
        }
    except Exception as error:
        # 返回与官方 Serving 一致的 5xx 结构，让 Go Consumer 按既有策略重试。
        return JSONResponse(
            status_code=500,
            content={"logId": log_id, "errorCode": 500, "errorMsg": str(error)},
        )
    finally:
        if image_path:
            try:
                os.remove(image_path)
            except FileNotFoundError:
                pass


def extract_texts_and_scores(predictions: list[Any], threshold: float) -> tuple[list[str], list[float]]:
    texts: list[str] = []
    scores: list[float] = []
    for prediction in predictions:
        payload = prediction.json
        if callable(payload):
            payload = payload()
        result = payload.get("res", payload)
        raw_texts = result.get("rec_texts", [])
        raw_scores = result.get("rec_scores", [])
        for text, score in zip(raw_texts, raw_scores):
            normalized_text = str(text).strip()
            normalized_score = float(score)
            if normalized_text and normalized_score >= threshold:
                texts.append(normalized_text)
                scores.append(normalized_score)
    return texts, scores
