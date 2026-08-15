package upgrade

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// 本机自签身份。有这个身份才 codesign；没有就跳过，不影响其他机器。
const localCodesignIdentity = "veightz-lark-bridge-codesign"

// 稳定 identifier。Go 默认 ad-hoc 签名是 a.out，每次重编 CDHash 都变，
// macOS 会把新二进制当成新 App，Downloads / 后台活动弹窗再来一遍。
const localCodesignIdentifier = "com.veightz.lark-coding-agent-bridge-go"

func maybeCodesign(bin string, out io.Writer) error {
	if !hasCodesignIdentity(localCodesignIdentity) {
		return nil
	}
	cmd := exec.Command("codesign", "--force", "--sign", localCodesignIdentity,
		"--identifier", localCodesignIdentifier, "--timestamp=none", bin)
	if data, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codesign 失败: %w\n%s", err, data)
	}
	fmt.Fprintf(out, "已用 %s 签名（%s）\n", localCodesignIdentity, localCodesignIdentifier)
	return nil
}

func hasCodesignIdentity(name string) bool {
	// 不带 -v：自签证书在没被标成 trusted 时不算 "valid"，但 codesign -s 仍可用。
	cmd := exec.Command("security", "find-identity", "-p", "codesigning")
	data, err := cmd.Output()
	if err != nil {
		return false
	}
	return identityListed(string(data), name)
}

func identityListed(findIdentityOutput, name string) bool {
	for _, line := range strings.Split(findIdentityOutput, "\n") {
		if strings.Contains(line, `"`+name+`"`) {
			return true
		}
	}
	return false
}
