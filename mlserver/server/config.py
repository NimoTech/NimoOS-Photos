"""Runtime settings, all overridable via MLSERVER_* environment variables."""
import os
from dataclasses import dataclass, field
from pathlib import Path

@dataclass(frozen=True)
class Settings:
    cache_dir: Path = field(default_factory=lambda: Path(os.environ.get("MLSERVER_CACHE", "/cache")))
    model_ttl_s: int = int(os.environ.get("MLSERVER_TTL", "300"))
    ttl_poll_s: int = int(os.environ.get("MLSERVER_TTL_POLL", "10"))
    device: str = os.environ.get("MLSERVER_DEVICE", "auto")  # auto | cpu | gpu | gpu.N

settings = Settings()
