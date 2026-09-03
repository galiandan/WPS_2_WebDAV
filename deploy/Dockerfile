FROM python:3.12-slim

WORKDIR /app
COPY src /app/src
COPY LICENSE README.md pyproject.toml /app/

ENV PYTHONPATH=/app/src \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

ENTRYPOINT ["python3", "-m", "wps_adapter", "serve"]
