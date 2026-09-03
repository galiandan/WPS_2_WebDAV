"""Experiment-first WPS enterprise adapter package."""

__version__ = "0.4.0"

from .client import (
    CredentialSource,
    DownloadStream,
    FileCredentialSource,
    ListPage,
    StaticCredentialSource,
    UploadOptions,
    WpsApiError,
    WpsClientConfig,
    WpsCredentials,
    WpsDriveClient,
)
from .storage import WpsStorage

__all__ = [
    "DownloadStream",
    "CredentialSource",
    "FileCredentialSource",
    "ListPage",
    "StaticCredentialSource",
    "UploadOptions",
    "WpsApiError",
    "WpsClientConfig",
    "WpsCredentials",
    "WpsDriveClient",
    "WpsStorage",
    "__version__",
]
