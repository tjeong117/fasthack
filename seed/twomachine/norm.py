import re, sys
TMP = re.compile(rb'/var/folders/[^\s"\':,)]+|/tmp/[A-Za-z0-9._\-]*[0-9A-Za-z]{6,}[^\s"\':,)]*')
ADDR = re.compile(rb'0x[0-9a-f]{6,16}')
DUR  = re.compile(rb'\b\d+\.\d+\s?(s|ms|sec|seconds)\b')
PID  = re.compile(rb'\bpid[= ]\d+', re.I)
def norm(b, root, home):
    b = b.replace(root.encode(), b'{{ROOT}}')
    b = b.replace(home.encode(), b'{{HOME}}')
    b = TMP.sub(b'{{TMP}}', b); b = ADDR.sub(b'{{ADDR}}', b)
    b = DUR.sub(b'{{DUR}}', b); b = PID.sub(b'{{PID}}', b)
    return b
if __name__ == "__main__":
    sys.stdout.buffer.write(norm(open(sys.argv[1],'rb').read(), sys.argv[2], sys.argv[3]))
