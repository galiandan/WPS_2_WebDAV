ARG BASE_IMAGE=python:3.12-slim
FROM ${BASE_IMAGE}

ARG APP_UID=1000
ARG APP_GID=1000

WORKDIR /app
COPY src /app/src
COPY LICENSE README.md pyproject.toml /app/

ENV PYTHONPATH=/app/src \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    APP_UID=${APP_UID} \
    APP_GID=${APP_GID}

RUN mkdir -p /var/lib/wps-adapter/uploads \
    && chown -R ${APP_UID}:${APP_GID} /var/lib/wps-adapter

# Run with the invoking host user's numeric identity so the mounted secret
# directory does not need to be writable by root inside the container.
USER ${APP_UID}:${APP_GID}

ENTRYPOINT ["python3", "-m", "wps_adapter", "serve"]
