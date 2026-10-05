package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yunzaixi-dev/tjucli/internal/account"
	"github.com/yunzaixi-dev/tjucli/internal/librarysync"
)

// Account and library commands: sign in, then work on a library as a local
// working copy (clone, status, diff, pull, push), the way one works with git.
// Another agent can do the same with TJUCLAW_TOKEN instead of signing in.

var libraryCommands = map[string]bool{
	"login": true, "logout": true, "whoami": true, "libraries": true,
	"clone": true, "status": true, "diff": true, "pull": true, "push": true, "resolve": true,
}

func errorID(err error) string {
	var remote *account.RemoteError
	switch {
	case errors.As(err, &remote):
		return remote.ID
	case errors.Is(err, account.ErrSignedOut):
		return "signed_out"
	case errors.Is(err, librarysync.ErrNotWorkingCopy):
		return "not_a_working_copy"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	for _, r := range msg {
		if !(r >= 'a' && r <= 'z' || r == '_') {
			return "library_sync_failed"
		}
	}
	return msg
}

func (r runner) explain(id string) {
	hints := map[string]string{
		"signed_out":             "还没有登录。运行 tjuclaw login，或设置环境变量 TJUCLAW_TOKEN。",
		"cli_token_invalid":      "登录已失效或令牌已被吊销。请重新运行 tjuclaw login。",
		"cli_token_scope_denied": "这个令牌只能读写知识库。",
		"not_a_working_copy":     "当前目录不是知识库工作副本。先运行 tjuclaw clone <知识库>。",
		"library_not_found":      "没有找到这个知识库。运行 tjuclaw libraries 查看可用的知识库。",
		"library_name_ambiguous": "有多个同名知识库，请改用知识库 ID。",
		"clone_target_not_empty": "目标目录不是空的。",
		"api_unreachable":        "无法连接 TJUClaw，请检查网络。",
		"login_denied":           "登录请求被拒绝。",
		"login_expired":          "验证码已过期，请重新运行 tjuclaw login。",
		"no_conflict":            "这个文件没有待解决的冲突。",
	}
	if hint, ok := hints[id]; ok {
		_, _ = fmt.Fprintln(r.errOut, hint)
	}
}

func (r runner) fail(err error) int {
	id := errorID(err)
	r.explain(id)
	return r.respond(nil, id)
}

func (r runner) client(dir string) (*account.Client, error) {
	creds, err := account.Load(dir)
	if err != nil {
		return nil, err
	}
	return account.NewClient(creds), nil
}

func (r runner) library(ctx context.Context, dir string, args []string) int {
	switch args[0] {
	case "login":
		return r.login(ctx, dir, args[1:])
	case "logout":
		if len(args) != 1 {
			return r.respond(nil, "usage_required")
		}
		creds, err := account.Load(dir)
		if err == nil && os.Getenv("TJUCLAW_TOKEN") == "" {
			// Revoke on the server too; a network failure still signs out here.
			_ = account.NewClient(creds).Do(ctx, "DELETE", "/cli/token", nil, nil, nil)
		}
		if err := account.Remove(dir); err != nil {
			return r.respond(nil, "logout_failed")
		}
		_, _ = fmt.Fprintln(r.errOut, "已退出登录，令牌已吊销。")
		return r.respond(map[string]bool{"signed_out": true}, "")
	case "whoami":
		client, err := r.client(dir)
		if err != nil {
			return r.fail(err)
		}
		var out struct {
			Email  string   `json:"email"`
			Scopes []string `json:"scopes"`
		}
		if err := client.Do(ctx, "GET", "/cli/whoami", nil, nil, &out); err != nil {
			return r.fail(err)
		}
		_, _ = fmt.Fprintf(r.errOut, "已登录为 %s（权限：知识库读写）\n", out.Email)
		return r.respond(map[string]any{"email": out.Email, "scopes": out.Scopes, "api": client.API}, "")
	case "libraries":
		client, err := r.client(dir)
		if err != nil {
			return r.fail(err)
		}
		libs, err := librarysync.ListLibraries(ctx, client)
		if err != nil {
			return r.fail(err)
		}
		for _, lib := range libs {
			role := ""
			if lib.Role != "" && lib.Role != "owner" {
				role = "（订阅，只读）"
			}
			_, _ = fmt.Fprintf(r.errOut, "%s  %s%s\n", lib.ID, lib.Name, role)
		}
		return r.respond(map[string]any{"libraries": libs}, "")
	case "clone":
		if len(args) < 2 || len(args) > 3 {
			_, _ = fmt.Fprintln(r.errOut, "用法：tjuclaw clone <知识库名称或 ID> [目录]")
			return r.respond(nil, "usage_required")
		}
		client, err := r.client(dir)
		if err != nil {
			return r.fail(err)
		}
		lib, err := librarysync.ResolveLibrary(ctx, client, args[1])
		if err != nil {
			return r.fail(err)
		}
		target := lib.Name
		if len(args) == 3 {
			target = args[2]
		}
		copy, res, err := librarysync.Clone(ctx, client, lib, target)
		if err != nil {
			return r.fail(err)
		}
		_, _ = fmt.Fprintf(r.errOut, "已克隆「%s」到 %s：%d 项\n", lib.Name, copy.Root, len(res.Added))
		return r.respond(map[string]any{"root": copy.Root, "library": lib, "pull": res}, "")
	}
	// The rest work inside a working copy.
	at, rest, err := splitDir(args[1:])
	if err != nil {
		return r.respond(nil, "usage_required")
	}
	if at == "" {
		at = "."
	}
	client, err := r.client(dir)
	if err != nil {
		return r.fail(err)
	}
	copy, err := librarysync.Find(at, client)
	if err != nil {
		return r.fail(err)
	}
	switch args[0] {
	case "status":
		changes, err := copy.Status()
		if err != nil {
			return r.fail(err)
		}
		r.printChanges(changes)
		return r.respond(map[string]any{"root": copy.Root, "library": copy.State.LibraryName, "changes": changes}, "")
	case "diff":
		return r.diff(copy, rest)
	case "pull":
		res, err := copy.Pull(ctx)
		if err != nil {
			return r.fail(err)
		}
		r.printPull(res)
		return r.respond(res, "")
	case "push":
		dryRun := false
		for _, a := range rest {
			if a != "--dry-run" {
				return r.respond(nil, "usage_required")
			}
			dryRun = true
		}
		res, err := copy.Push(ctx, dryRun)
		if err != nil {
			return r.fail(err)
		}
		r.printPush(res)
		return r.respond(res, "")
	case "resolve":
		if len(rest) != 1 {
			_, _ = fmt.Fprintln(r.errOut, "用法：tjuclaw resolve <文件>")
			return r.respond(nil, "usage_required")
		}
		rel, err := relativeTo(copy.Root, rest[0])
		if err != nil {
			return r.respond(nil, "usage_required")
		}
		if err := copy.Resolve(rel); err != nil {
			return r.fail(err)
		}
		_, _ = fmt.Fprintf(r.errOut, "已标记 %s 为已解决；下次 push 会用本地内容覆盖远端版本。\n", rel)
		return r.respond(map[string]string{"resolved": rel}, "")
	}
	return r.respond(nil, "unknown_command")
}

// splitDir takes "-C <dir>" out of the arguments: the working copy to use.
func splitDir(args []string) (dir string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" {
			if i+1 >= len(args) || dir != "" {
				return "", nil, errors.New("usage_required")
			}
			dir = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return dir, rest, nil
}

func relativeTo(root, p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("outside_working_copy")
	}
	return filepath.ToSlash(rel), nil
}

func (r runner) login(ctx context.Context, dir string, args []string) int {
	noBrowser := false
	if _, err := parseFlags(args, func(fs *flag.FlagSet) { fs.BoolVar(&noBrowser, "no-browser", false, "") }); err != nil {
		return r.respond(nil, "usage_required")
	}
	base, err := account.APIBase()
	if err != nil {
		return r.respond(nil, "invalid_api_url")
	}
	client := account.NewClient(account.Credentials{API: base})
	host, _ := os.Hostname()
	name := "tjuclaw"
	if host != "" {
		name += " @ " + host
	}
	device, err := client.StartLogin(ctx, name)
	if err != nil {
		return r.fail(err)
	}
	_, _ = fmt.Fprintf(r.errOut, "在浏览器中打开下面的链接，确认验证码后即可登录：\n  %s\n验证码：%s\n等待确认…（%d 分钟内有效，Ctrl+C 取消）\n",
		device.CompleteURI, device.Code, max(device.ExpiresIn/60, 1))
	if !noBrowser {
		openBrowser(device.CompleteURI)
	}
	creds, err := client.Wait(ctx, device)
	if err != nil {
		if errors.Is(err, account.ErrDenied) {
			return r.fail(errors.New("login_denied"))
		}
		if errors.Is(err, account.ErrExpired) {
			return r.fail(errors.New("login_expired"))
		}
		return r.fail(err)
	}
	if err := account.Save(dir, creds); err != nil {
		return r.respond(nil, "credentials_save_failed")
	}
	_, _ = fmt.Fprintf(r.errOut, "已登录为 %s（权限：知识库读写）。令牌保存在 %s。\n", creds.Email, filepath.Join(dir, "auth.json"))
	return r.respond(map[string]any{"email": creds.Email, "api": creds.API}, "")
}

// openBrowser starts the system browser when there is a desktop to show it.
func openBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", link)
	}
	_ = cmd.Start()
}

func (r runner) printChanges(changes []librarysync.Change) {
	if len(changes) == 0 {
		_, _ = fmt.Fprintln(r.errOut, "没有改动。")
		return
	}
	labels := map[string]string{"added": "新增", "modified": "修改", "deleted": "删除", "moved": "移动"}
	notes := map[string]string{
		"rich_text_read_only": "（富文本笔记只读，不会推送）", "conflict": "（有冲突，见 .tjuclaw/conflicts，合并后运行 tjuclaw resolve）",
		"too_large": "（超过大小限制，不会推送）", "empty_file": "（空文件，不会推送）",
	}
	for _, ch := range changes {
		target := ch.Path
		if ch.Kind == librarysync.KindFolder {
			target += "/"
		}
		if ch.Op == "moved" {
			target = ch.From + " -> " + ch.Path
		}
		_, _ = fmt.Fprintf(r.errOut, "  %s  %s%s\n", labels[ch.Op], target, notes[ch.Note])
	}
}

func (r runner) printPull(res librarysync.PullResult) {
	total := len(res.Added) + len(res.Updated) + len(res.Removed) + len(res.Moved)
	_, _ = fmt.Fprintf(r.errOut, "已拉取：新增 %d，更新 %d，移动 %d，删除 %d。\n", len(res.Added), len(res.Updated), len(res.Moved), len(res.Removed))
	for _, p := range res.Conflicts {
		_, _ = fmt.Fprintf(r.errOut, "  冲突  %s：本地和远端都改了，远端版本存在 .tjuclaw/conflicts/%s\n", p, p)
	}
	for _, p := range res.Kept {
		_, _ = fmt.Fprintf(r.errOut, "  保留  %s：远端已删除，但本地有修改；下次 push 会重新创建它\n", p)
	}
	if total == 0 && len(res.Conflicts) == 0 && len(res.Kept) == 0 {
		_, _ = fmt.Fprintln(r.errOut, "已是最新。")
	}
}

func (r runner) printPush(res librarysync.PushResult) {
	_, _ = fmt.Fprintf(r.errOut, "已推送：新建 %d，修改 %d，移动 %d，删除 %d。\n", len(res.Created), len(res.Updated), len(res.Moved), len(res.Deleted))
	for _, p := range res.Conflicts {
		_, _ = fmt.Fprintf(r.errOut, "  冲突  %s：远端在你上次同步后被改过，未推送。先运行 tjuclaw pull。\n", p)
	}
	for _, ch := range res.Skipped {
		_, _ = fmt.Fprintf(r.errOut, "  跳过  %s（%s）\n", ch.Path, ch.Note)
	}
}

func (r runner) diff(copy *librarysync.Copy, paths []string) int {
	changes, err := copy.Status()
	if err != nil {
		return r.fail(err)
	}
	want := map[string]bool{}
	for _, p := range paths {
		rel, err := relativeTo(copy.Root, p)
		if err != nil {
			return r.respond(nil, "usage_required")
		}
		want[rel] = true
	}
	var b strings.Builder
	for _, ch := range changes {
		if len(want) > 0 && !want[ch.Path] {
			continue
		}
		if ch.Kind == librarysync.KindFolder {
			continue
		}
		if ch.Kind == librarysync.KindFile {
			if ch.Op != "moved" {
				fmt.Fprintf(&b, "%s 文件 %s（二进制，不显示内容差异）\n", map[string]string{"added": "新增", "modified": "修改", "deleted": "删除"}[ch.Op], ch.Path)
			}
			continue
		}
		var before, after []byte
		if t, ok := copy.State.Entries[ch.Path]; ok {
			before, _ = copy.Base(t.ID)
		} else if t, ok := copy.State.Entries[ch.From]; ok {
			before, _ = copy.Base(t.ID)
		}
		if ch.Op != "deleted" {
			after, _ = os.ReadFile(filepath.Join(copy.Root, filepath.FromSlash(ch.Path)))
		}
		from, to := "a/"+ch.Path, "b/"+ch.Path
		if ch.Op == "moved" {
			from = "a/" + ch.From
		}
		if ch.Op == "added" {
			from = "/dev/null"
		}
		if ch.Op == "deleted" {
			to = "/dev/null"
		}
		b.WriteString(librarysync.UnifiedDiff(string(before), string(after), from, to))
	}
	_, _ = fmt.Fprint(r.errOut, b.String())
	return r.respond(map[string]any{"diff": b.String(), "changes": changes}, "")
}
