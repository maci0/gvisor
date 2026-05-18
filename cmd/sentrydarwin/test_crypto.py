"""Minimal reproducer for HVF crypto instruction traps (EC=0)."""
import os, sys

def test(name, fn):
    try:
        result = fn()
        os.write(1, f"PASS {name}: {result}\n".encode())
    except Exception as e:
        os.write(1, f"FAIL {name}: {e}\n".encode())
    os.fsync(1)

# Test 1: CRC32 (uses CRC32 instruction, not crypto extension)
test("crc32", lambda: __import__('binascii').crc32(b'test'))

# Test 2: hashlib SHA256 (uses AESE/AESMC via OpenSSL)
test("sha256", lambda: __import__('hashlib').sha256(b'test').hexdigest()[:8])

# Test 3: hashlib MD5 (may not use crypto extensions)
test("md5", lambda: __import__('hashlib').md5(b'test').hexdigest()[:8])

# Test 4: hmac (wraps hashlib)
test("hmac", lambda: __import__('hmac').new(b'key', b'msg', 'sha256').hexdigest()[:8])

os.write(1, b"done\n")
os.fsync(1)
