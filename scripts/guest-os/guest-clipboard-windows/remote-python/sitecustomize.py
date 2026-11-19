"""Keep Python's private directories accessible to their own AppContainer.

Windows mkdir(mode=0o700) grants the user/admins but omits the restricted package
SID. Keep that private DACL and add only this process's actual package identity.
Loaded on the remote shell's PYTHONPATH, never in the privileged bridge.
"""
import os


def _configure():
    if os.name != "nt":
        return
    import ctypes
    from ctypes import wintypes

    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    security = ctypes.WinDLL("advapi32", use_last_error=True)
    kernel.GetCurrentProcess.restype = wintypes.HANDLE
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel.LocalFree.argtypes = [ctypes.c_void_p]
    security.OpenProcessToken.argtypes = [wintypes.HANDLE, wintypes.DWORD, ctypes.POINTER(wintypes.HANDLE)]
    security.GetTokenInformation.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)]
    security.ConvertSidToStringSidW.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)]
    security.ConvertStringSecurityDescriptorToSecurityDescriptorW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p]

    token = wintypes.HANDLE()
    if not security.OpenProcessToken(kernel.GetCurrentProcess(), 8, ctypes.byref(token)):
        raise ctypes.WinError(ctypes.get_last_error())

    def token_info(kind):
        needed = wintypes.DWORD()
        security.GetTokenInformation(token, kind, None, 0, ctypes.byref(needed))
        if not 0 < needed.value <= 65536:
            raise ctypes.WinError(ctypes.get_last_error())
        buffer = ctypes.create_string_buffer(needed.value)
        if not security.GetTokenInformation(token, kind, buffer, needed.value, ctypes.byref(needed)):
            raise ctypes.WinError(ctypes.get_last_error())
        return buffer

    def token_sid(kind):
        buffer = token_info(kind)
        pointer = ctypes.c_void_p.from_buffer(buffer).value
        text = ctypes.c_void_p()
        if not pointer or not security.ConvertSidToStringSidW(pointer, ctypes.byref(text)):
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            return ctypes.wstring_at(text)
        finally:
            kernel.LocalFree(text)

    try:
        if not wintypes.DWORD.from_buffer(token_info(29)).value:  # TokenIsAppContainer
            return
        user, package = token_sid(1), token_sid(31)
    finally:
        kernel.CloseHandle(token)

    class Attributes(ctypes.Structure):
        _fields_ = [("length", wintypes.DWORD), ("descriptor", ctypes.c_void_p), ("inherit", wintypes.BOOL)]

    kernel.CreateDirectoryW.argtypes = [wintypes.LPCWSTR, ctypes.POINTER(Attributes)]
    descriptor = ctypes.c_void_p()
    dacl = f"D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;{user})(A;OICI;FA;;;{package})"
    if not security.ConvertStringSecurityDescriptorToSecurityDescriptorW(dacl, 1, ctypes.byref(descriptor), None):
        raise ctypes.WinError(ctypes.get_last_error())
    # Copy before releasing LocalAlloc; the closure owns the buffer for life.
    security.GetSecurityDescriptorLength.argtypes = [ctypes.c_void_p]
    security.GetSecurityDescriptorLength.restype = wintypes.DWORD
    try:
        saved_descriptor = ctypes.create_string_buffer(ctypes.string_at(descriptor, security.GetSecurityDescriptorLength(descriptor)))
    finally:
        kernel.LocalFree(descriptor)
    original = os.mkdir

    def mkdir(path, mode=0o777, *, dir_fd=None):
        if mode != 0o700 or dir_fd is not None:
            return original(path, mode, dir_fd=dir_fd)
        import sys
        name = os.fsdecode(os.fspath(path))
        if "\0" in name:
            raise ValueError("embedded null character")
        sys.audit("os.mkdir", path, mode, -1)
        attributes = Attributes(ctypes.sizeof(Attributes), ctypes.addressof(saved_descriptor), False)
        if not kernel.CreateDirectoryW(name, ctypes.byref(attributes)):
            error = ctypes.WinError(ctypes.get_last_error())
            error.filename = name
            raise error

    os.mkdir = mkdir


_configure()
del _configure
