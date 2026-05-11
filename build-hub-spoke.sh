#!/bin/bash
# Build OpenBao with hub init and spoke join commands

set -e

echo "==> Building OpenBao binary with hub/spoke commands..."
cd /home/rudro25/go/src/github.com/openbao/openbao
make dev

echo "==> Building plugin-runner..."
cd plugins/database/remote-db-plugin
make plugin-runner

echo "==> Copying plugin-runner to bin directory..."
cp bin/plugin-runner /home/rudro25/go/src/github.com/openbao/openbao/bin/

echo "==> Testing bao commands..."
cd /home/rudro25/go/src/github.com/openbao/openbao
./bin/bao hub init --help
./bin/bao spoke join --help

echo ""
echo "✅ Build successful!"
echo ""
echo "Next steps:"
echo "1. Build Docker image:"
echo "   docker build --build-arg BIN_NAME=bao -t rudro25/openbao:plug-tst8 -f Dockerfile ."
echo ""
echo "2. Push to registry:"
echo "   docker push rudro25/openbao:plug-tst8"
echo ""
echo "3. Update VaultServer version to plug-tst8"
echo ""
echo "4. Test commands:"
echo "   kubectl exec -it vault5-0 -n demo -- /bin/bao hub init --port=50053"
echo "   kubectl exec -it spoke-agent-pod -n demo -- /bin/bao spoke join --server=10.2.0.88:30496 --name=spoke-1"
