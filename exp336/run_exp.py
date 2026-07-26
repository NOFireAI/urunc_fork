#!/usr/bin/env python3
import socket, subprocess, sys, os, time, json, threading, signal

KERNEL="/usr/lib/aarch64-linux-gnu/proxmox-backup/file-restore/Image"

def log(m): print(m, flush=True)

def drain(sock, sink, stop):
    buf=b""
    sock.settimeout(0.5)
    while not stop.is_set():
        try:
            d=sock.recv(4096)
            if not d: break
            buf+=d
            while b"\n" in buf:
                line,buf=buf.split(b"\n",1)
                line=line.strip()
                if line:
                    sink.append((round(time.time(),2), line.decode("utf-8","replace")))
        except socket.timeout:
            continue
        except OSError:
            break

def wait_serial(path, marker, timeout):
    t0=time.time()
    while time.time()-t0<timeout:
        try:
            with open(path,"r",errors="replace") as f:
                if marker in f.read(): return True
        except FileNotFoundError:
            pass
        time.sleep(1)
    return False

def main():
    mode=sys.argv[1]          # ignore | react | poweroff
    initrd=sys.argv[2]
    serial=f"serial-{mode}.log"
    qsock=f"qmp-{mode}.sock"
    for p in (serial,qsock):
        try: os.remove(p)
        except FileNotFoundError: pass

    cmd=["qemu-system-aarch64","-M","virt","-cpu","max","-m","512",
         "-kernel",KERNEL,"-initrd",initrd,
         "-append","console=ttyAMA0 rdinit=/init panic=-1",
         "-no-reboot","-display","none","-serial",f"file:{serial}",
         "-qmp",f"unix:{qsock},server,nowait","-nic","none"]
    log("CMD: "+" ".join(cmd))
    qemu=subprocess.Popen(cmd)
    log(f"qemu pid={qemu.pid}")

    marker={"ignore":"BOOTED-IGNORER","react":"BOOTED-REACTOR","poweroff":"BOOTED-POWEROFF-NOW"}[mode]
    if not wait_serial(serial, marker, 120):
        log("!! guest did not reach marker "+marker)
        qemu.kill(); return
    log(f">>> guest reached {marker}")

    # For poweroff mode, guest triggers shutdown itself; just watch QMP+exit.
    # Connect QMP
    for _ in range(20):
        try:
            s=socket.socket(socket.AF_UNIX); s.connect(qsock); break
        except OSError: time.sleep(0.3)
    events=[]; stop=threading.Event()
    th=threading.Thread(target=drain,args=(s,events,stop)); th.start()
    time.sleep(0.5)
    def send(obj):
        s.sendall((json.dumps(obj)+"\r\n").encode()); time.sleep(0.4)
    # handshake
    send({"execute":"qmp_capabilities"})
    time.sleep(0.5)

    if mode in ("ignore","react"):
        log("--- sending system_powerdown ---")
        send({"execute":"system_powerdown"})
        time.sleep(1)

    # Watch for up to 35s: SHUTDOWN event / guest serial reaction / process exit
    exit_code=None
    t0=time.time()
    while time.time()-t0<35:
        rc=qemu.poll()
        if rc is not None:
            exit_code=rc; log(f">>> QEMU EXITED after {round(time.time()-t0,1)}s, exit_code={rc}")
            break
        time.sleep(1)

    if mode=="ignore" and exit_code is None:
        log("--- QEMU still alive after 35s (expected for ignorer); sending 2nd system_powerdown ---")
        send({"execute":"system_powerdown"})
        time.sleep(3)
        rc=qemu.poll()
        log(f">>> after 2nd powerdown, poll={rc} (None=still running)")

    stop.set(); th.join(timeout=2)
    try: s.close()
    except: pass

    log("=== QMP TRAFFIC ===")
    for ts,line in events:
        log(f"[{ts}] {line}")

    if qemu.poll() is None:
        log("=== QEMU still running -> terminating ===")
        qemu.terminate()
        try: qemu.wait(timeout=5)
        except: qemu.kill()
    else:
        log(f"=== final exit_code={qemu.poll()} ===")

    log("=== SERIAL TAIL (last 25 lines) ===")
    try:
        with open(serial,errors="replace") as f:
            for l in f.read().splitlines()[-25:]: log(l)
    except FileNotFoundError: pass

main()
