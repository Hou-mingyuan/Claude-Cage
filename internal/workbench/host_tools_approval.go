package workbench

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sylvinus/agent-vm/internal/desktop"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

const (
	hostApprovalWait       = 2 * time.Minute
	hostApprovalCommandMax = 4000
)

// hostApprovals asks the user before each command Claude runs on this
// computer and each call to a plugin's tool. "Always" is remembered in
// grants, for one project's commands or one plugin, until the user takes it
// back.
type hostApprovals struct {
	ask       func(ctx context.Context, title, msg string) (desktop.Scope, bool)
	remoteAsk func(ctx context.Context, prompt remoteApprovalPrompt) (desktop.Scope, bool)
	// grants is nil where nothing is remembered beyond this start.
	grants *hostGrants
	// permissions are those each loaded plugin declares.
	permissions map[string][]string
	// unasked are the plugins whose tools need no approval.
	unasked map[string]bool
	// neverAlways lists "plugin/tool" keys that must always be confirmed per
	// call even when the plugin itself has an "always allow" grant.
	neverAlways map[string]bool
	// dialog keeps one question on screen at a time.
	dialog  sync.Mutex
	mu      sync.Mutex
	allowed map[string]bool
}

func newHostApprovals() *hostApprovals {
	return &hostApprovals{ask: desktop.AskScope, allowed: map[string]bool{}, unasked: map[string]bool{}, neverAlways: map[string]bool{}}
}

func (a *hostApprovals) approve(ctx context.Context, request hostbridge.CommandRequest) error {
	prompt := remoteApprovalPrompt{
		Kind:    "command",
		Project: request.Project,
		Dir:     request.Dir,
		Command: request.Command,
	}
	return a.decide(ctx, hostGrantProject+request.Project, []string{"commands"}, request.Command+request.Dir, len(request.Command),
		"允许 Claude 在这台电脑上运行命令？", hostApprovalMessage(request), prompt)
}

// approveTool asks about one call to a plugin's tool. "Always" covers that
// plugin for as long as it declares no further permission.
func (a *hostApprovals) approveTool(ctx context.Context, request hostbridge.ToolRequest) error {
	if a.unasked[request.Plugin] {
		return nil
	}
	forceAsk := a.neverAlways[request.Plugin+"/"+request.Tool]
	title, source := "允许 Claude 使用这台电脑上的插件？", ""
	if request.FromView {
		title, source = "允许插件页面使用它的工具？", "\n来源　插件自己的页面"
	}
	prompt := remoteApprovalPrompt{
		Kind:      "tool",
		Plugin:    request.Plugin,
		Tool:      request.Tool,
		Arguments: request.Arguments,
		FromView:  request.FromView,
	}
	footer := "\n\n插件以你的用户权限运行。「始终允许」会记住对该插件的授权，以后不再询问；可以在插件页随时撤销。"
	if forceAsk {
		footer = "\n\n此敏感控制操作不支持始终允许，每次执行均需由你确认。"
	}
	return a.decideWithOptions(ctx, hostGrantPlugin+request.Plugin, a.permissions[request.Plugin], request.Arguments, len(request.Arguments),
		title,
		"插件「"+request.Plugin+"」\n工具　"+request.Tool+source+"\n\n"+request.Arguments+footer, prompt, forceAsk)
}

// unshowableRequest is a request refused before anyone is asked, because the
// dialog could not show it in full.
type unshowableRequest string

func (e unshowableRequest) Error() string { return string(e) }

// decide asks unless key was already allowed. shown is the text the user
// must be able to read in full to judge the request.
func (a *hostApprovals) decide(ctx context.Context, key string, permissions []string, shown string, length int, title, message string, prompt remoteApprovalPrompt) error {
	return a.decideWithOptions(ctx, key, permissions, shown, length, title, message, prompt, false)
}

func (a *hostApprovals) decideWithOptions(ctx context.Context, key string, permissions []string, shown string, length int, title, message string, prompt remoteApprovalPrompt, forceAsk bool) error {
	// A request the dialog cannot show faithfully is one the user cannot
	// judge: control and invisible format characters can hide part of it.
	if strings.ContainsFunc(shown, func(r rune) bool {
		return (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r)
	}) {
		return unshowableRequest("the request contains control or invisible characters and was not run")
	}
	if length > hostApprovalCommandMax {
		return unshowableRequest("the request is too long to show for approval and was not run; put long content in a file")
	}
	if !forceAsk && a.alwaysAllowed(key, permissions) {
		return nil
	}
	a.dialog.Lock()
	defer a.dialog.Unlock()
	// Another call may have been answered for good while this one waited.
	if !forceAsk && a.alwaysAllowed(key, permissions) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hostApprovalWait)
	defer cancel()
	var scope desktop.Scope
	var answered bool
	if a.remoteAsk != nil {
		scope, answered = a.remoteAsk(ctx, prompt)
	}
	if !answered && a.ask != nil {
		scope, answered = a.ask(ctx, title, message)
	}
	switch {
	case !answered:
		return errors.New("no approval dialog could be shown on the user's computer, so nothing was run")
	case scope == desktop.Always:
		if !forceAsk {
			// Kept for this start even if it cannot be written down.
			a.mu.Lock()
			a.allowed[key] = true
			a.mu.Unlock()
			if a.grants != nil {
				_ = a.grants.grant(key, permissions)
			}
		}
	case scope != desktop.Once:
		return errors.New("the user did not approve this request, so nothing was run")
	}
	return nil
}

func (a *hostApprovals) alwaysAllowed(key string, permissions []string) bool {
	a.mu.Lock()
	allowed := a.allowed[key]
	a.mu.Unlock()
	return allowed || a.grants != nil && a.grants.allowed(key, permissions)
}

func hostApprovalMessage(request hostbridge.CommandRequest) string {
	return "项目「" + filepath.Base(request.Project) + "」\n目录　" + request.Dir + "\n\n" + request.Command +
		"\n\n命令以你的用户权限运行，可以访问这台电脑上的任何内容。「始终允许」会记住对该项目的授权，以后在这个项目里运行命令不再询问；可以在插件页随时撤销。"
}
