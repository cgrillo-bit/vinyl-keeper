import io
import logging
import os
import time

import numpy as np
import onnxruntime as ort
from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from PIL import Image

# Normalization values per model family.
# https://github.com/openai/CLIP/blob/main/clip/clip.py#L79-L80
# https://pytorch.org/vision/stable/models.html
NORM_VALUES = {
    "clip": {
        "mean": [0.48145466, 0.4578275, 0.40821073],
        "std": [0.26862954, 0.26130258, 0.27577711],
    },
    "imagenet": {
        "mean": [0.485, 0.456, 0.406],
        "std": [0.229, 0.224, 0.225],
    },
}

model_path = os.environ.get("EMBED_MODEL_PATH", "")
model_family = os.environ.get("EMBED_MODEL_FAMILY", "clip")
embed_dim = int(os.environ.get("EMBED_DIM", "512"))
image_size = int(os.environ.get("EMBED_IMAGE_SIZE", "224"))
log_level = os.environ.get("LOG_LEVEL", "INFO").upper()

logging.basicConfig(
    level=getattr(logging, log_level, logging.INFO),
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger("image_service")

if model_family not in NORM_VALUES:
    raise ValueError(
        f"unknown EMBED_MODEL_FAMILY: {model_family}, expected one of {list(NORM_VALUES.keys())}"
    )

norm = NORM_VALUES[model_family]

if not model_path:
    raise ValueError("EMBED_MODEL_PATH is required")

session = ort.InferenceSession(model_path)

app = FastAPI(title="vinyl-keeper-image-service")


def service_info() -> dict:
    model_inputs = session.get_inputs()
    model_outputs = session.get_outputs()
    return {
        "service": app.title,
        "model_path": model_path,
        "model_file": os.path.basename(model_path),
        "model_family": model_family,
        "embed_dim": embed_dim,
        "image_size": image_size,
        "providers": session.get_providers(),
        "model_input_names": [i.name for i in model_inputs],
        "model_output_names": [o.name for o in model_outputs],
    }


@app.on_event("startup")
def startup_log() -> None:
    logger.info("Image service started")
    logger.info("Configured image_size=%d", image_size)


def preprocess(img_bytes: bytes) -> np.ndarray:
    """Decode raw image bytes, resize, normalize, return [1, 3, H, W] float32 array."""
    img = Image.open(io.BytesIO(img_bytes)).convert("RGB")
    img = img.resize((image_size, image_size), Image.BICUBIC)

    # [H, W, 3] uint8 -> float32 0-1
    arr = np.array(img, dtype=np.float32) / 255.0

    # Normalize per channel
    mean = np.array(norm["mean"], dtype=np.float32)
    std = np.array(norm["std"], dtype=np.float32)
    arr = (arr - mean) / std

    # [H, W, 3] -> [1, 3, H, W]
    arr = np.transpose(arr, (2, 0, 1))
    arr = np.expand_dims(arr, axis=0)

    return arr


@app.get("/health")
def health():
    return JSONResponse(content={"ok": True, "service": app.title}, status_code=200)


@app.get("/info")
def info() -> JSONResponse:
    return JSONResponse(content=service_info(), status_code=200)


@app.post("/embed")
async def embed(request: Request) -> Response:
    start = time.perf_counter()
    logger.info("/embed request received")
    raw = await request.body()
    if len(raw) == 0:
        logger.warning("/embed received empty image body")
        return Response(content=b"empty image", status_code=400)

    try:
        tensor = preprocess(raw)
        result = session.run(["embedding"], {"image": tensor})
        embedding = result[0][0]
    except Exception:
        logger.exception("/embed failed")
        return Response(content=b"embedding failed", status_code=500)

    # Convert to float64 little-endian bytes
    embedding_f64 = embedding.astype(np.float64)
    elapsed_ms = (time.perf_counter() - start) * 1000.0
    logger.info("/embed request completed in %.1fms", elapsed_ms)

    return Response(
        content=embedding_f64.tobytes(), media_type="application/octet-stream"
    )
