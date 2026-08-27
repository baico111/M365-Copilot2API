#!/usr/bin/env python3
"""M365 Copilot2API 管理脚本 — 支持 Linux / macOS / Windows

用法:
    python manage.py start       # 启动服务（后台）
    python manage.py stop        # 停止服务
    python manage.py restart     # 重启服务
    python manage.py status      # 查看运行状态
    python manage.py logs [N]    # 查看最近 N 行日志（默认 20）
    python manage.py err [N]     # 查看最近 N 行错误日志（默认 20）

环境变量:
    M365_ADMIN_PASSWORD  管理员密码（默认 admin123，生产环境务必修改）
    M365_LISTEN          监听地址（默认 0.0.0.0:4141）
    M365_MASTER_KEY      Token 加密主密钥（强烈建议设置）
    M365_DATA_DIR        数据目录（默认脚本目录下的 data/）
"""
import subprocess
import sys
import time
import os
import signal
import platform
import secrets
import base64

# 基于脚本自身位置推导路径，跨平台兼容
BASE_DIR = os.path.dirname(os.path.abspath(__file__))

# 自动选择可执行文件名（Windows 用 .exe，其他平台无后缀）
if sys.platform == 'win32':
    SERVER_EXE = os.path.join(BASE_DIR, "m365-copilot2api.exe")
else:
    SERVER_EXE = os.path.join(BASE_DIR, "m365-copilot2api")

SERVER_DIR = BASE_DIR
LOG_FILE = os.path.join(BASE_DIR, "server.log")
ERR_FILE = os.path.join(BASE_DIR, "server-error.log")
PID_FILE = os.path.join(BASE_DIR, "server.pid")
DATA_DIR = os.environ.get("M365_DATA_DIR", os.path.join(BASE_DIR, "data"))


def get_pid():
    try:
        with open(PID_FILE, 'r') as f:
            return int(f.read().strip())
    except:
        return None


def is_running(pid):
    try:
        os.kill(pid, 0)
        return True
    except:
        return False


def generate_master_key():
    """生成一个安全的随机主密钥"""
    return base64.b64encode(secrets.token_bytes(32)).decode('ascii')


def start():
    pid = get_pid()
    if pid and is_running(pid):
        print(f"Server already running (PID {pid})")
        return

    # 检查可执行文件是否存在
    if not os.path.exists(SERVER_EXE):
        print(f"ERROR: Binary not found at {SERVER_EXE}")
        print("Please build first:")
        if sys.platform == 'win32':
            print("  go build -o m365-copilot2api.exe ./cmd/server")
        else:
            print("  go build -o m365-copilot2api ./cmd/server")
        sys.exit(1)

    env = os.environ.copy()
    admin_pw = env.get("M365_ADMIN_PASSWORD", "admin123")
    listen_addr = env.get("M365_LISTEN", "0.0.0.0:4141")

    # 确保数据目录存在
    os.makedirs(DATA_DIR, exist_ok=True)

    # 如果未设置 M365_MASTER_KEY，生成一个并提示
    if not env.get("M365_MASTER_KEY") and not env.get("M365_TOKEN_ENCRYPTION_KEY"):
        generated_key = generate_master_key()
        env["M365_MASTER_KEY"] = generated_key
        print(f"[security] Generated M365_MASTER_KEY for this session.")
        print(f"[security] To persist it, set M365_MASTER_KEY in your environment or .env file:")
        print(f"[security]   export M365_MASTER_KEY={generated_key}")
        print()

    env.update({
        "M365_LISTEN": listen_addr,
        "M365_DATA_DIR": DATA_DIR,
        "M365_CONFIG": os.path.join(DATA_DIR, "accounts.json"),
        "M365_TOKEN_CACHE": os.path.join(DATA_DIR, "token-cache.json"),
        "M365_SESSION_CACHE": os.path.join(DATA_DIR, "sessions.json"),
        "M365_API_KEYS": os.path.join(DATA_DIR, "api-keys.json"),
        "M365_ADMIN_PASSWORD": admin_pw,
        "M365_CLEANUP_MODE": env.get("M365_CLEANUP_MODE", "keep_n"),
        "M365_CLEANUP_KEEP_N": env.get("M365_CLEANUP_KEEP_N", "3"),
    })

    log = open(LOG_FILE, 'a')
    err = open(ERR_FILE, 'a')
    log.write(f"\n--- Server starting at {time.strftime('%Y-%m-%d %H:%M:%S')} ---\n")
    log.flush()

    if sys.platform == 'win32':
        proc = subprocess.Popen(
            [SERVER_EXE],
            cwd=SERVER_DIR,
            env=env,
            stdout=log,
            stderr=err,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.DETACHED_PROCESS
        )
    else:
        # Linux / macOS — 用 setsid 实现守护进程
        proc = subprocess.Popen(
            [SERVER_EXE],
            cwd=SERVER_DIR,
            env=env,
            stdout=log,
            stderr=err,
            start_new_session=True
        )

    with open(PID_FILE, 'w') as f:
        f.write(str(proc.pid))

    time.sleep(3)

    if is_running(proc.pid):
        print(f"Server started (PID {proc.pid})")
        print(f"Listening on http://{listen_addr}")
        print(f"Logs: {LOG_FILE}")
    else:
        print("Server failed to start!")
        print("--- Error log ---")
        try:
            with open(ERR_FILE, 'r') as f:
                content = f.read()
                print(content[-2000:] if len(content) > 2000 else content)
        except:
            print("(no error log available)")


def stop():
    pid = get_pid()
    if not pid:
        print("No server running")
        return

    try:
        os.kill(pid, signal.SIGTERM)
        print(f"Sending SIGTERM to PID {pid}...")
        time.sleep(2)
        if is_running(pid):
            print("Process still running, sending SIGKILL...")
            os.kill(pid, signal.SIGKILL)
            time.sleep(1)
        print(f"Server stopped (PID {pid})")
    except ProcessLookupError:
        print(f"Process {pid} already gone")
    except Exception as e:
        print(f"Error stopping: {e}")

    try:
        os.remove(PID_FILE)
    except:
        pass


def restart():
    stop()
    time.sleep(1)
    start()


def status():
    pid = get_pid()
    if pid and is_running(pid):
        print(f"Server running (PID {pid})")
        listen_addr = os.environ.get("M365_LISTEN", "0.0.0.0:4141")
        print(f"Listening on http://{listen_addr}")
    else:
        print("Server not running")


def logs(lines=20):
    try:
        with open(LOG_FILE, 'r') as f:
            all_lines = f.readlines()
            print(''.join(all_lines[-lines:]))
    except FileNotFoundError:
        print("No log file. Server may not have been started yet.")
    except Exception as e:
        print(f"Error reading log: {e}")


def err_logs(lines=20):
    try:
        with open(ERR_FILE, 'r') as f:
            all_lines = f.readlines()
            print(''.join(all_lines[-lines:]))
    except FileNotFoundError:
        print("No error log file. Server may not have been started yet.")
    except Exception as e:
        print(f"Error reading error log: {e}")


def show_help():
    print("M365 Copilot2API Management Script")
    print(f"Platform: {platform.system()} {platform.machine()}")
    print(f"Binary:  {SERVER_EXE}")
    print(f"Data:    {DATA_DIR}")
    print()
    print("Usage:")
    print("  python manage.py start       Start server in background")
    print("  python manage.py stop        Stop server")
    print("  python manage.py restart     Restart server")
    print("  python manage.py status      Show server status")
    print("  python manage.py logs [N]    Show last N log lines (default 20)")
    print("  python manage.py err [N]    Show last N error log lines (default 20)")
    print("  python manage.py genkey      Generate a random M365_MASTER_KEY")
    print("  python manage.py help        Show this help message")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        show_help()
        sys.exit(1)

    cmd = sys.argv[1].lower()
    if cmd == "start":
        start()
    elif cmd == "stop":
        stop()
    elif cmd == "restart":
        restart()
    elif cmd == "status":
        status()
    elif cmd == "logs":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 20
        logs(n)
    elif cmd == "err":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 20
        err_logs(n)
    elif cmd == "genkey":
        print(f"M365_MASTER_KEY={generate_master_key()}")
    elif cmd in ("help", "-h", "--help"):
        show_help()
    else:
        print(f"Unknown command: {cmd}")
        show_help()
        sys.exit(1)
