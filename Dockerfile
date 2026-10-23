FROM python:3.12-slim
WORKDIR /app
COPY app.py /app/app.py
COPY static /app/static
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOST=0.0.0.0 PORT=8787 MEDIA_ROOT=/media DATA_DIR=/data
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8787/health', timeout=3)" || exit 1
CMD ["python", "app.py"]
