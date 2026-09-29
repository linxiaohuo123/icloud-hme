FROM python:3.11-slim-bookworm

# 安装 Camoufox / Playwright 依赖库
RUN apt-get update && apt-get install -y --no-install-recommends \
    wget \
    curl \
    ca-certificates \
    libglib2.0-0 \
    libnss3 \
    libnspr4 \
    libatk1.0-0 \
    libatk-bridge2.0-0 \
    libgtk-3-0 \
    libcups2 \
    libdrm2 \
    libdbus-1-3 \
    libxcb1 \
    libxkbcommon0 \
    libx11-6 \
    libxcomposite1 \
    libxdamage1 \
    libxext6 \
    libxfixes3 \
    libxrandr2 \
    libpango-1.0-0 \
    libcairo2 \
    libasound2 \
    fonts-liberation \
    xvfb \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Linux 容器保留 Linux 字体及登录流程使用的 Windows 指纹字体。
RUN python3 -m camoufox fetch \
    && browser_cache="$(python3 -c 'from camoufox.pkgman import INSTALL_DIR; print(INSTALL_DIR)')" \
    && find "$browser_cache/browsers" -type d -path '*/fonts/macos' -prune -exec rm -rf {} +

COPY main.py .
COPY healthcheck.py .

EXPOSE 8089

CMD ["python3", "main.py"]
