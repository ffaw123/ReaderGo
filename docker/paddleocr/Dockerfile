# 使用官方 PaddlePaddle CPU 基础镜像，避免在项目镜像中自行编译推理框架。
FROM paddlepaddle/paddle:3.3.0

# 固定依赖版本范围，服务入口显式关闭 MKLDNN，规避 PaddlePaddle 3.3 CPU PIR 转换异常。
RUN python -m pip install --no-cache-dir paddleocr==3.4.1 fastapi "uvicorn[standard]"

WORKDIR /app
COPY server.py /app/server.py

EXPOSE 8080

# 保持 OCR Consumer 所需的 POST /ocr 契约，同时提供 /health 容器健康检查。
CMD ["python", "-m", "uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080"]
