MODULE=rpc-gateway

BUILD=./build
MODULES=$(MODULE)
CLEAN_MODULES=clean-$(MODULE)
LDFLAGS="-X 'drlib/common.BuildTime=`date`' -X 'drlib/common.GitHash=`git rev-parse HEAD`'"

all: $(MODULES)


clean: $(CLEAN_MODULES)

clean-%:
	rm -f $(BUILD)/dr_$*d
	rm -f $(BUILD)/dr_$*d.linux

%:
	@echo "Compling module $@"
	go build -ldflags $(LDFLAGS) -o $(BUILD)/dr_$@d .
	#脚本是假定在MAC下编译的，所以编译linux二进制文件使用交叉编译
	#CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=x86_64-linux-musl-gcc CGO_LDFLAGS="-static" go build -ldflags $(LDFLAGS) -o $(BUILD)/$@/dr_$@d.linux ./service/$@
	GOOS=linux GOARCH=amd64 go build -ldflags $(LDFLAGS) -o $(BUILD)/dr_$@d.linux .


.PHONY: all clean