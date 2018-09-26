package main

import (
	"fmt"
	"image"
	"image/color"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/image/font"

	"github.com/aarzilli/nucular"
	"github.com/aarzilli/nucular/clipboard"
	"github.com/aarzilli/nucular/label"
	"github.com/aarzilli/nucular/rect"
	nstyle "github.com/aarzilli/nucular/style"

	"github.com/aarzilli/gdlv/internal/dlvclient/service/api"
)

type numberMode int

const (
	decMode numberMode = iota
	hexMode
	octMode
)

type Variable struct {
	*api.Variable
	Width    int
	Value    string
	IntMode  numberMode
	FloatFmt string
	loading  bool
	Varname  string

	ShortType   string
	DisplayName string
	Expression  string

	Children []*Variable
}

func wrapApiVariableSimple(v *api.Variable) *Variable {
	return wrapApiVariable(v, v.Name, v.Name, false)
}

func wrapApiVariable(v *api.Variable, name, expr string, customFormatters bool) *Variable {
	r := &Variable{Variable: v}
	r.Value = v.Value
	r.Expression = expr
	if f := varFormat[v.Addr]; f != nil {
		f(r)
	} else if (v.Kind == reflect.Int || v.Kind == reflect.Uint) && ((v.Type == "uint8") || (v.Type == "int32")) {
		n, _ := strconv.Atoi(v.Value)
		if n >= ' ' && n <= '~' {
			r.Value = fmt.Sprintf("%s %q", v.Value, n)
		}
	} else if f := conf.CustomFormatters[v.Type]; f != nil && customFormatters {
		f.Format(r)
	}

	if name != "" {
		r.DisplayName = name
	} else {
		r.DisplayName = v.Type
	}

	r.ShortType = shortenType(v.Type)

	r.Varname = r.DisplayName

	r.Children = wrapApiVariables(v.Children, v.Kind, 0, r.Expression, customFormatters)

	if v.Kind == reflect.Interface {
		if len(r.Children) > 0 && r.Children[0].Kind == reflect.Ptr {
			if len(r.Children[0].Children) > 0 {
				r.Children[0].Children[0].DisplayName = r.Children[0].DisplayName
			}
		}
	}
	return r
}

func wrapApiVariables(vs []api.Variable, kind reflect.Kind, start int, expr string, customFormatters bool) []*Variable {
	r := make([]*Variable, 0, len(vs))

	const minInlineKeyValueLen = 20

	if kind == reflect.Map {
		for i := 0; i < len(vs); i += 2 {
			ok := false
			key, value := &vs[i], &vs[i+1]
			if len(key.Children) == 0 && len(key.Value) < minInlineKeyValueLen {
				var keyname string
				switch key.Kind {
				case reflect.String:
					keyname = fmt.Sprintf("[%q]", key.Value)
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.Float32, reflect.Float64:
					keyname = fmt.Sprintf("[%s]", key.Value)
				}
				if keyname != "" {
					r = append(r, wrapApiVariable(value, keyname, "", customFormatters))
					r = append(r, nil)
					ok = true
				}
			}

			if !ok {
				r = append(r, wrapApiVariable(key, fmt.Sprintf("[%d key]", start+i/2), "", customFormatters))
				r = append(r, wrapApiVariable(value, fmt.Sprintf("[%d value]", start+i/2), "", customFormatters))
			}
		}
		return r
	}

	for i := range vs {
		var childName, childExpr string
		switch kind {
		case reflect.Interface:
			childName = "data"
			childExpr = ""
		case reflect.Slice, reflect.Array:
			childName = fmt.Sprintf("[%d]", start+i)
			if expr != "" {
				childExpr = fmt.Sprintf("%s[%d]", expr, start+i)
			}
		case reflect.Ptr:
			childName = vs[i].Name
			if expr != "" {
				childExpr = fmt.Sprintf("(*(%s))", expr)
			}
		case reflect.Struct, reflect.Chan:
			childName = vs[i].Name
			if expr != "" {
				x := expr
				if strings.HasPrefix(x, "(*(") && strings.HasSuffix(x, "))") {
					x = x[3 : len(x)-2]
				}
				childExpr = fmt.Sprintf("%s.%s", x, vs[i].Name)
			}
		case 0:
			childName = vs[i].Name
			childExpr = vs[i].Name

		default:
			childName = vs[i].Name
			childExpr = ""
		}
		r = append(r, wrapApiVariable(&vs[i], childName, childExpr, customFormatters))
	}
	return r
}

var globalsPanel = struct {
	asyncLoad    asyncLoad
	filterEditor nucular.TextEditor
	showAddr     bool
	fullTypes    bool
	globals      []*Variable
}{
	filterEditor: nucular.TextEditor{Filter: spacefilter},
}

var localsPanel = struct {
	asyncLoad    asyncLoad
	filterEditor nucular.TextEditor
	showAddr     bool
	fullTypes    bool
	locals       []*Variable

	expressions []Expr
	selected    int
	ed          nucular.TextEditor
	v           []*Variable
}{
	filterEditor: nucular.TextEditor{Filter: spacefilter},
	selected:     -1,
	ed:           nucular.TextEditor{Flags: nucular.EditSelectable | nucular.EditSigEnter | nucular.EditClipboard},
}

type Expr struct {
	Expr                         string
	pinnedGid                    int
	pinnedFrameOffset            int64
	maxArrayValues, maxStringLen int
	traced                       bool
}

func loadGlobals(p *asyncLoad) {
	globals, err := client.ListPackageVariables("", getVariableLoadConfig())
	globalsPanel.globals = wrapApiVariables(globals, 0, 0, "", true)
	sort.Sort(variablesByName(globalsPanel.globals))
	p.done(err)
}

func updateGlobals(container *nucular.Window) {
	w := globalsPanel.asyncLoad.showRequest(container)
	if w == nil {
		return
	}

	w.MenubarBegin()
	w.Row(varRowHeight).Static(90, 0, 100, 100)
	w.Label("Filter:", "LC")
	globalsPanel.filterEditor.Edit(w)
	filter := string(globalsPanel.filterEditor.Buffer)
	w.CheckboxText("Full Types", &globalsPanel.fullTypes)
	w.CheckboxText("Address", &globalsPanel.showAddr)
	w.MenubarEnd()

	globals := globalsPanel.globals

	for i := range globals {
		if strings.Index(globals[i].Name, filter) >= 0 {
			showVariable(w, 0, globalsPanel.showAddr, globalsPanel.fullTypes, -1, globals[i])
		}
	}
}

type variablesByName []*Variable

func (vars variablesByName) Len() int           { return len(vars) }
func (vars variablesByName) Swap(i, j int)      { vars[i], vars[j] = vars[j], vars[i] }
func (vars variablesByName) Less(i, j int) bool { return vars[i].Name < vars[j].Name }

func loadLocals(p *asyncLoad) {
	args, errloc := client.ListFunctionArgs(api.EvalScope{curGid, curFrame}, getVariableLoadConfig())
	localsPanel.locals = wrapApiVariables(args, 0, 0, "", true)
	locals, errarg := client.ListLocalVariables(api.EvalScope{curGid, curFrame}, getVariableLoadConfig())
	for i := range locals {
		v := &locals[i]
		if v.Kind == reflect.Ptr && len(v.Name) > 1 && v.Name[0] == '&' && len(v.Children) > 0 {
			name := v.Name[1:]
			locals[i] = v.Children[0]
			locals[i].Name = name
		}
	}
	localsPanel.locals = append(localsPanel.locals, wrapApiVariables(locals, 0, 0, "", true)...)

	hasDeclLine := false
	for i := range localsPanel.locals {
		if localsPanel.locals[i].DeclLine != 0 {
			hasDeclLine = true
		}
	}

	if hasDeclLine {
		sort.Slice(localsPanel.locals, func(i, j int) bool { return localsPanel.locals[i].DeclLine < localsPanel.locals[j].DeclLine })
	}

	varmap := map[string]int{}

	for i := range localsPanel.locals {
		varname := localsPanel.locals[i].Varname
		d := varmap[varname]
		localsPanel.locals[i].Varname += fmt.Sprintf(" %d", d)
		d++
		varmap[varname] = d
	}

	var scrollbackOut = editorWriter{&scrollbackEditor, true}
	for i := range localsPanel.expressions {
		loadOneExpr(i)
		if localsPanel.expressions[i].traced {
			fmt.Fprintf(&scrollbackOut, "%s = %s\n", localsPanel.v[i].Name, localsPanel.v[i].SinglelineString(true, false))
		}
	}

	for _, err := range []error{errarg, errloc} {
		if err != nil {
			p.done(err)
			return
		}
	}
	p.done(nil)
}

const (
	varRowHeight    = 20
	varEditorHeight = 25
	posRowHeight    = 36
	moreBtnWidth    = 70
)

func updateLocals(container *nucular.Window) {
	w := localsPanel.asyncLoad.showRequest(container)
	if w == nil {
		return
	}

	w.MenubarBegin()
	w.Row(varRowHeight).Static(90, 0, 100, 100)
	w.Label("Filter:", "LC")
	localsPanel.filterEditor.Edit(w)
	filter := string(localsPanel.filterEditor.Buffer)
	w.CheckboxText("Full Types", &localsPanel.fullTypes)
	w.CheckboxText("Address", &localsPanel.showAddr)
	w.MenubarEnd()

	locals := localsPanel.locals

	if len(localsPanel.expressions) > 0 {
		if w.TreePush(nucular.TreeTab, "Expression", true) {
			for i := 0; i < len(localsPanel.expressions); i++ {
				if i == localsPanel.selected {
					exprsEditor(w)
				} else {
					if localsPanel.v[i] == nil {
						w.Row(varRowHeight).Dynamic(1)
						w.Label(fmt.Sprintf("loading %s", localsPanel.expressions[i].Expr), "LC")
					} else {
						showVariable(w, 0, localsPanel.showAddr, localsPanel.fullTypes, i, localsPanel.v[i])
					}
				}
			}
			w.TreePop()
		}
	}

	if len(locals) > 0 {
		if w.TreePush(nucular.TreeTab, "Local variables and arguments", true) {
			for i := range locals {
				if strings.Index(locals[i].Name, filter) >= 0 {
					showVariable(w, 0, localsPanel.showAddr, localsPanel.fullTypes, -1, locals[i])
				}
			}
			w.TreePop()
		}
	}
}

func isPinned(expr string) bool {
	return expr[0] == '['
}

func findFrameOffset(gid int, frameOffset int64) (frame int) {
	frames, err := client.Stacktrace(gid, 100, nil)
	if err != nil {
		return -1
	}

	for i := range frames {
		if frames[i].FrameOffset == frameOffset {
			return i
		}
	}
	return -1
}

func loadOneExpr(i int) {
	expr := localsPanel.expressions[i].Expr
	gid, frame := curGid, curFrame
	if localsPanel.expressions[i].pinnedGid > 0 {
		gid = localsPanel.expressions[i].pinnedGid
		frame = findFrameOffset(localsPanel.expressions[i].pinnedGid, localsPanel.expressions[i].pinnedFrameOffset)
		if frame < 0 {
			localsPanel.v[i] = wrapApiVariable(&api.Variable{Name: "(pinned) " + expr, Unreadable: "could not find frame"}, "", "", true)
			return
		}
	}
	cfg := getVariableLoadConfig()
	if localsPanel.expressions[i].maxArrayValues > 0 {
		cfg.MaxArrayValues = localsPanel.expressions[i].maxArrayValues
		cfg.MaxStringLen = localsPanel.expressions[i].maxStringLen
	}
	v, err := client.EvalVariable(api.EvalScope{gid, frame}, expr, cfg)
	if err != nil {
		v = &api.Variable{Unreadable: err.Error()}
	}
	v.Name = expr
	if localsPanel.expressions[i].pinnedGid > 0 {
		v.Name = "(pinned) " + v.Name
	}
	localsPanel.v[i] = wrapApiVariable(v, v.Name, v.Name, true)
}

func exprsEditor(w *nucular.Window) {
	w.Row(varEditorHeight).Dynamic(1)
	active := localsPanel.ed.Edit(w)
	if active&nucular.EditCommitted == 0 {
		return
	}

	newexpr := string(localsPanel.ed.Buffer)
	localsPanel.ed.Buffer = localsPanel.ed.Buffer[:0]
	localsPanel.ed.Cursor = 0
	localsPanel.ed.Active = true
	localsPanel.ed.CursorFollow = true

	if localsPanel.selected < 0 {
		addExpression(newexpr)
	} else {
		localsPanel.expressions[localsPanel.selected].Expr = newexpr
		go func(i int) {
			additionalLoadMu.Lock()
			defer additionalLoadMu.Unlock()
			loadOneExpr(i)
		}(localsPanel.selected)
		localsPanel.selected = -1
	}
}

func addExpression(newexpr string) {
	localsPanel.expressions = append(localsPanel.expressions, Expr{Expr: newexpr})
	localsPanel.v = append(localsPanel.v, nil)
	i := len(localsPanel.v) - 1
	go func(i int) {
		additionalLoadMu.Lock()
		defer additionalLoadMu.Unlock()
		loadOneExpr(i)
	}(i)
}

func showExprMenu(parentw *nucular.Window, exprMenuIdx int, v *Variable, clipb []byte) {
	if client.Running() {
		return
	}
	w := parentw.ContextualOpen(0, image.Point{}, parentw.LastWidgetBounds, nil)
	if w == nil {
		return
	}
	w.Row(20).Dynamic(1)
	if fn := detailsAvailable(v); fn != nil {
		if w.MenuItem(label.TA("Details", "LC")) {
			fn(w.Master(), v.Expression)
		}
	}

	if w.MenuItem(label.TA("Copy to clipboard", "LC")) {
		clipboard.Set(string(clipb))
	}

	if exprMenuIdx >= 0 && exprMenuIdx < len(localsPanel.expressions) {
		pinned := localsPanel.expressions[exprMenuIdx].pinnedGid > 0
		if w.MenuItem(label.TA("Edit expression", "LC")) {
			localsPanel.selected = exprMenuIdx
			localsPanel.ed.Buffer = []rune(localsPanel.expressions[localsPanel.selected].Expr)
			localsPanel.ed.Cursor = len(localsPanel.ed.Buffer)
			localsPanel.ed.CursorFollow = true
			localsPanel.ed.Active = true
			commandLineEditor.Active = false
		}
		if w.MenuItem(label.TA("Remove expression", "LC")) {
			if exprMenuIdx+1 < len(localsPanel.expressions) {
				copy(localsPanel.expressions[exprMenuIdx:], localsPanel.expressions[exprMenuIdx+1:])
				copy(localsPanel.v[exprMenuIdx:], localsPanel.v[exprMenuIdx+1:])
			}
			localsPanel.expressions = localsPanel.expressions[:len(localsPanel.expressions)-1]
			localsPanel.v = localsPanel.v[:len(localsPanel.v)-1]
		}
		if w.MenuItem(label.TA("Load parameters...", "LC")) {
			w.Master().PopupOpen(fmt.Sprintf("Load parameters for %s", localsPanel.expressions[exprMenuIdx].Expr), dynamicPopupFlags, rect.Rect{100, 100, 400, 700}, true, configureLoadParameters(exprMenuIdx))
		}
		if w.CheckboxText("Pin to frame", &pinned) {
			if pinned && curFrame < len(stackPanel.stack) {
				localsPanel.expressions[exprMenuIdx].pinnedGid = curGid
				localsPanel.expressions[exprMenuIdx].pinnedFrameOffset = stackPanel.stack[curFrame].FrameOffset
			} else {
				localsPanel.expressions[exprMenuIdx].pinnedGid = 0
			}
			go func(i int) {
				additionalLoadMu.Lock()
				defer additionalLoadMu.Unlock()
				loadOneExpr(i)
			}(exprMenuIdx)
		}
		if exprMenuIdx < len(localsPanel.expressions) {
			w.CheckboxText("Traced", &localsPanel.expressions[exprMenuIdx].traced)
		}
	} else if v.Expression != "" {
		if w.MenuItem(label.TA("Add as expression", "LC")) {
			addExpression(v.Expression)

		}
	}

	if v.Kind == reflect.Func {
		if w.MenuItem(label.TA("Go to definition", "LC")) {
			locs, err := client.FindLocation(api.EvalScope{curGid, curFrame}, fmt.Sprintf("*%#x", v.Base))
			if err == nil && len(locs) == 1 {
				listingPanel.pinnedLoc = &locs[0]
				go refreshState(refreshToSameFrame, clearNothing, nil)
			}
		}
	}

	switch v.Kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		mode := v.IntMode
		oldmode := mode
		if w.OptionText("Hexadecimal", mode == hexMode) {
			mode = hexMode
		}
		if w.OptionText("Octal", mode == octMode) {
			mode = octMode
		}
		if w.OptionText("Decimal", mode == decMode) {
			mode = decMode
		}
		if mode != oldmode {
			f := intFormatter[mode]
			varFormat[v.Addr] = f
			f(v)
			v.Width = 0
		}

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		mode := v.IntMode
		oldmode := mode
		if w.OptionText("Hexadecimal", mode == hexMode) {
			mode = hexMode
		}
		if w.OptionText("Octal", mode == octMode) {
			mode = octMode
		}
		if w.OptionText("Decimal", mode == decMode) {
			mode = decMode
		}
		if mode != oldmode {
			f := uintFormatter[mode]
			varFormat[v.Addr] = f
			f(v)
			v.Width = 0
		}

	case reflect.Float32, reflect.Float64:
		if w.MenuItem(label.TA("Format...", "LC")) {
			newFloatViewer(w, v)
		}
	}

	switch v.Type {
	case "bool", "int", "int8", "int16", "int32", "int64", "byte", "rune":
	case "uintptr", "uint", "uint8", "uint16", "uint32", "uint64":
	case "float32", "float64", "complex64", "complex128":
	case "string":
	default:
		if cfmt := conf.CustomFormatters[v.Type]; cfmt != nil {
			if w.MenuItem(label.TA("Edit custom formatter...", "LC")) {
				viewCustomFormatterMaker(w, v, cfmt.Fmtstr, cfmt.Argstr)
			}
			if w.MenuItem(label.TA("Remove custom formatter", "LC")) {
				delete(conf.CustomFormatters, v.Type)
				saveConfiguration()
				go refreshState(refreshToSameFrame, clearFrameSwitch, nil)
			}
		} else {
			if w.MenuItem(label.TA("Custom format for type...", "LC")) {
				viewCustomFormatterMaker(w, v, "", []string{})
			}
		}
	}

	switch v.Kind {
	case reflect.Slice, reflect.Array:
		if v.Addr != 0 {
			if w.MenuItem(label.TA("Find element...", "LC")) {
				viewFindElement(w, v)
			}
		}
	}

	if w.MenuItem(label.TA("Location...", "LC")) {
		out := editorWriter{&scrollbackEditor, false}
		fmt.Fprintf(&out, "location of %q at %#x: %s\n", v.Name, curPC, v.LocationExpr)
	}
}

const maxVariableHeaderWidth = 4096

func variableHeader(w *nucular.Window, addr, fullTypes bool, exprMenu int, v *Variable) bool {
	style := w.Master().Style()

	w.LayoutSetWidthScaled(maxVariableHeaderWidth)
	lblrect, out, isopen := w.TreePushCustom(nucular.TreeNode, v.Varname, false)
	if out == nil {
		return isopen
	}

	clipb := []byte{}

	print := func(str string, font font.Face) {
		clipb = append(clipb, []byte(str)...)
		clipb = append(clipb, ' ')
		out.DrawText(lblrect, str, font, style.Tab.Text)
		width := nucular.FontWidth(font, str) + spaceWidth
		lblrect.X += width
		lblrect.W -= width
	}

	if addr {
		print(fmt.Sprintf("%#x", v.Addr), style.Font)
	}
	if isopen {
		print(v.DisplayName, boldFace)

		switch v.Kind {
		case reflect.Slice:
			print(getDisplayType(v, fullTypes), style.Font)
			print(fmt.Sprintf("(len: %d cap: %d)", v.Len, v.Cap), style.Font)
		case reflect.Interface:
			if len(v.Children) > 0 && v.Children[0] != nil {
				print(fmt.Sprintf("%s (%v)", getDisplayType(v, fullTypes), getDisplayType(v.Children[0], fullTypes)), style.Font)
			} else {
				print(v.Type, style.Font)
			}
		default:
			print(getDisplayType(v, fullTypes), style.Font)
		}
	} else {
		print(v.DisplayName, boldFace)
		print(getDisplayType(v, fullTypes), style.Font)
		if v.Value != "" {
			print("= "+v.Value, style.Font)
		} else {
			print("= "+v.SinglelineString(false, fullTypes), style.Font)
		}
	}
	showExprMenu(w, exprMenu, v, clipb)
	return isopen
}

func variableNoHeader(w *nucular.Window, addr, fullTypes bool, exprMenu int, v *Variable, value string) {
	style := w.Master().Style()
	symX := style.Tab.Padding.X
	symW := nucular.FontHeight(style.Font)
	item_spacing := style.NormalWindow.Spacing
	z := symX + symW + item_spacing.X + 2*style.Tab.Spacing.X
	w.LayoutSetWidthScaled(z)
	w.Spacing(1)
	w.LayoutSetWidthScaled(maxVariableHeaderWidth)

	//w.Label(fmt.Sprintf("%s %s = %s", v.DisplayName, v.Type, value), "LC")

	lblrect, out := w.Custom(nstyle.WidgetStateActive)
	if out == nil {
		return
	}

	lblrect.Y += style.Text.Padding.Y

	clipb := []byte{}

	print := func(str string, font font.Face) {
		clipb = append(clipb, []byte(str)...)
		clipb = append(clipb, ' ')
		out.DrawText(lblrect, str, font, style.Text.Color)
		width := nucular.FontWidth(font, str) + spaceWidth
		lblrect.X += width
		lblrect.W -= width
	}

	if addr {
		print(fmt.Sprintf("%#x", v.Addr), style.Font)
	}
	print(v.DisplayName, boldFace)
	print(getDisplayType(v, fullTypes), style.Font)
	print("= "+value, style.Font)

	showExprMenu(w, exprMenu, v, clipb)
}

func getDisplayType(v *Variable, fullTypes bool) string {
	if fullTypes {
		return v.Type
	}
	return v.ShortType
}

func showVariable(w *nucular.Window, depth int, addr, fullTypes bool, exprMenu int, v *Variable) {
	style := w.Master().Style()

	if v.Flags&api.VariableShadowed != 0 || v.Unreadable != "" {
		savedStyle := *style
		defer func() {
			*style = savedStyle
		}()
		const darken = 0.75
		for _, p := range []*color.RGBA{&style.Text.Color, &style.Tab.NodeButton.TextNormal, &style.Tab.NodeButton.TextHover, &style.Tab.NodeButton.TextActive, &style.Tab.Text} {
			p.A = p.A / 2
			p.R = p.R / 2
			p.G = p.G / 2
			p.B = p.B / 2
		}
	}

	hdr := func() bool {
		return variableHeader(w, addr, fullTypes, exprMenu, v)
	}

	cblbl := func(fmtstr string, args ...interface{}) {
		variableNoHeader(w, addr, fullTypes, exprMenu, v, fmt.Sprintf(fmtstr, args...))
	}

	dynlbl := func(s string) {
		w.Row(varRowHeight).Dynamic(1)
		w.Label(s, "LC")
	}

	w.Row(varRowHeight).Static()
	if v.Unreadable != "" {
		cblbl("(unreadable %s)", v.Unreadable)
		return
	}

	if depth > 0 && v.Addr == 0 {
		cblbl("nil")
		return
	}

	switch v.Kind {
	case reflect.Slice:
		if hdr() {
			showArrayOrSliceContents(w, depth, addr, fullTypes, v)
			w.TreePop()
		}
	case reflect.Array:
		if hdr() {
			showArrayOrSliceContents(w, depth, addr, fullTypes, v)
			w.TreePop()
		}
	case reflect.Ptr:
		if len(v.Children) == 0 {
			cblbl("?")
		} else if v.Type == "" || v.Children[0].Addr == 0 {
			cblbl("nil")
		} else {
			if hdr() {
				if v.Children[0].OnlyAddr {
					loadMoreStruct(v.Children[0])
					dynlbl("Loading...")
				} else {
					showVariable(w, depth+1, addr, fullTypes, -1, v.Children[0])
				}
				w.TreePop()
			}
		}
	case reflect.UnsafePointer:
		cblbl("unsafe.Pointer(%#x)", v.Children[0].Addr)
	case reflect.String:
		if v.Len == int64(len(v.Value)) {
			cblbl("%q", v.Value)
		} else {
			cblbl("%q...", v.Value)
		}
	case reflect.Chan:
		if len(v.Children) == 0 {
			cblbl("nil")
		} else {
			if hdr() {
				showStructContents(w, depth, addr, fullTypes, v)
				w.TreePop()
			}
		}
	case reflect.Struct:
		if hdr() {
			if int(v.Len) != len(v.Children) && len(v.Children) == 0 {
				loadMoreStruct(v)
				dynlbl("Loading...")
			} else {
				showStructContents(w, depth, addr, fullTypes, v)
			}
			w.TreePop()
		}
	case reflect.Interface:
		if v.Children[0].Kind == reflect.Invalid {
			cblbl("nil")
		} else {
			if hdr() {
				showInterfaceContents(w, depth, addr, fullTypes, v)
				w.TreePop()
			}
		}
	case reflect.Map:
		if hdr() {
			if depth < 10 && !v.loading && len(v.Children) > 0 && autoloadMore(v.Children[0]) {
				v.loading = true
				loadMoreStruct(v)
			}
			for i := range v.Children {
				if v.Children[i] != nil {
					showVariable(w, depth+1, addr, fullTypes, -1, v.Children[i])
				}
			}
			if len(v.Children)/2 != int(v.Len) && v.Addr != 0 {
				w.Row(varRowHeight).Static(moreBtnWidth)
				if w.ButtonText(fmt.Sprintf("%d more", int(v.Len)-(len(v.Children)/2))) {
					loadMoreMap(v)
				}
			}
			w.TreePop()
		}
	case reflect.Func:
		if v.Value == "" {
			cblbl("nil")
		} else {
			cblbl(v.Value)
		}
	case reflect.Complex64, reflect.Complex128:
		cblbl("(%s + %si)", v.Children[0].Value, v.Children[1].Value)
	case reflect.Float32, reflect.Float64:
		cblbl(v.Value)
	default:
		if v.Value != "" {
			cblbl(v.Value)
		} else {
			cblbl("(unknown %s)", v.Kind)
		}
	}
}

func showArrayOrSliceContents(w *nucular.Window, depth int, addr, fullTypes bool, v *Variable) {
	if depth < 10 && !v.loading && len(v.Children) > 0 && autoloadMore(v.Children[0]) {
		v.loading = true
		loadMoreStruct(v)
	}
	for i := range v.Children {
		showVariable(w, depth+1, addr, fullTypes, -1, v.Children[i])
	}
	if len(v.Children) != int(v.Len) && v.Addr != 0 {
		w.Row(varRowHeight).Static(moreBtnWidth)
		if w.ButtonText(fmt.Sprintf("%d more", int(v.Len)-len(v.Children))) {
			loadMoreArrayOrSlice(v)
		}
	}
}

func autoloadMore(v *Variable) bool {
	if v.OnlyAddr {
		return true
	}
	if v.Kind == reflect.Struct && len(v.Children) == 0 {
		return true
	}
	if v.Kind == reflect.Ptr && len(v.Children) == 1 && v.Children[0].OnlyAddr {
		return true
	}
	return false
}

func showStructContents(w *nucular.Window, depth int, addr, fullTypes bool, v *Variable) {
	for i := range v.Children {
		showVariable(w, depth+1, addr, fullTypes, -1, v.Children[i])
	}
}

func showInterfaceContents(w *nucular.Window, depth int, addr, fullTypes bool, v *Variable) {
	if len(v.Children) <= 0 {
		return
	}
	data := v.Children[0]
	if data.OnlyAddr {
		loadMoreStruct(v)
		w.Row(varRowHeight).Dynamic(1)
		w.Label("Loading...", "LC")
		return
	}
	if data.Kind == reflect.Ptr {
		if len(data.Children) <= 0 {
			loadMoreStruct(v)
			w.Row(varRowHeight).Dynamic(1)
			w.Label("Loading...", "LC")
			return
		}
		data = data.Children[0]
	}

	switch data.Kind {
	case reflect.Struct:
		showStructContents(w, depth, addr, fullTypes, data)
	case reflect.Array, reflect.Slice:
		showArrayOrSliceContents(w, depth, addr, fullTypes, data)
	default:
		showVariable(w, depth+1, addr, fullTypes, -1, data)
	}
}

var additionalLoadMu sync.Mutex
var additionalLoadRunning bool

func loadMoreMap(v *Variable) {
	additionalLoadMu.Lock()
	defer additionalLoadMu.Unlock()

	if !additionalLoadRunning {
		additionalLoadRunning = true
		go func() {
			expr := fmt.Sprintf("(*(*%q)(%#x))[%d:]", v.Type, v.Addr, len(v.Children)/2)
			lv, err := client.EvalVariable(api.EvalScope{curGid, curFrame}, expr, LongArrayLoadConfig)
			if err != nil {
				out := editorWriter{&scrollbackEditor, true}
				fmt.Fprintf(&out, "Error loading array contents %s: %v\n", expr, err)
				// prevent further attempts at loading
				v.Len = int64(len(v.Children) / 2)
			} else {
				v.Children = append(v.Children, wrapApiVariables(lv.Children, reflect.Map, len(v.Children), v.Expression, true)...)
			}
			wnd.Changed()
			additionalLoadMu.Lock()
			additionalLoadRunning = false
			additionalLoadMu.Unlock()
		}()
	}
}

func loadMoreArrayOrSlice(v *Variable) {
	additionalLoadMu.Lock()
	defer additionalLoadMu.Unlock()
	if !additionalLoadRunning {
		additionalLoadRunning = true
		go func() {
			expr := fmt.Sprintf("(*(*%q)(%#x))[%d:]", v.Type, v.Addr, len(v.Children))
			lv, err := client.EvalVariable(api.EvalScope{curGid, curFrame}, expr, LongArrayLoadConfig)
			if err != nil {
				out := editorWriter{&scrollbackEditor, true}
				fmt.Fprintf(&out, "Error loading array contents %s: %v\n", expr, err)
				// prevent further attempts at loading
				v.Len = int64(len(v.Children))
			} else {
				v.Children = append(v.Children, wrapApiVariables(lv.Children, v.Kind, len(v.Children), v.Expression, true)...)
			}
			additionalLoadMu.Lock()
			additionalLoadRunning = false
			additionalLoadMu.Unlock()
			wnd.Changed()
		}()
	}
}

func loadMoreStruct(v *Variable) {
	additionalLoadMu.Lock()
	defer additionalLoadMu.Unlock()
	if !additionalLoadRunning {
		additionalLoadRunning = true
		go func() {
			lv, err := client.EvalVariable(api.EvalScope{curGid, curFrame}, fmt.Sprintf("*(*%q)(%#x)", v.Type, v.Addr), getVariableLoadConfig())
			if err != nil {
				v.Unreadable = err.Error()
			} else {
				dn := v.DisplayName
				vn := v.Varname
				lv.Name = v.Name
				*v = *wrapApiVariable(lv, lv.Name, v.Expression, true)
				v.Varname = vn
				v.DisplayName = dn
			}
			wnd.Changed()
			additionalLoadMu.Lock()
			additionalLoadRunning = false
			additionalLoadMu.Unlock()
		}()
	}
}

type openDetailsWindowFn func(nucular.MasterWindow, string)

func detailsAvailable(v *Variable) openDetailsWindowFn {
	if v == nil {
		return nil
	}
	switch v.Type {
	case "string", "[]uint8", "[]int32":
		return newDetailViewer
	case "[]int", "[]int8", "[]int16", "[]int64", "[]uint", "[]uint16", "[]uint32", "[]uint64":
		return newDetailViewer
	}
	return nil
}

func configureLoadParameters(exprMenuIdx int) func(w *nucular.Window) {
	expr := &localsPanel.expressions[exprMenuIdx]
	maxArrayValues := expr.maxArrayValues
	maxStringLen := expr.maxStringLen
	if maxArrayValues <= 0 {
		cfg := getVariableLoadConfig()
		maxArrayValues = cfg.MaxArrayValues
		maxStringLen = cfg.MaxStringLen
	}

	return func(w *nucular.Window) {
		commit := false
		w.Row(30).Static(0)
		w.PropertyInt("Max array load:", 0, &maxArrayValues, 4096, 1, 1)

		w.Row(30).Static(0)
		w.PropertyInt("Max string load:", 0, &maxStringLen, 4096, 1, 1)

		w.Row(30).Static(0, 100, 100)
		w.Spacing(1)
		if w.ButtonText("Cancel") {
			w.Close()
		}
		if w.ButtonText("OK") {
			commit = true
		}
		if commit {
			expr.maxArrayValues = maxArrayValues
			expr.maxStringLen = maxStringLen
			loadOneExpr(exprMenuIdx)
			w.Close()
		}
	}
}

func shortenType(typ string) string {
	out, ok := shortenTypeEx(typ)
	if !ok {
		return typ
	}
	return out
}

func shortenTypeEx(typ string) (string, bool) {
	switch {
	case strings.HasPrefix(typ, "[]"):
		sub, ok := shortenTypeEx(typ[2:])
		return "[]" + sub, ok
	case strings.HasPrefix(typ, "*"):
		sub, ok := shortenTypeEx(typ[1:])
		return "*" + sub, ok
	case strings.HasPrefix(typ, "map["):
		depth := 1
		for i := 4; i < len(typ); i++ {
			switch typ[i] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					key, keyok := shortenTypeEx(typ[4:i])
					val, valok := shortenTypeEx(typ[i+1:])
					return "map[" + key + "]" + val, keyok && valok
				}
			}
		}
		return "", false
	case typ == "interface {}" || typ == "interface{}":
		return typ, true
	case typ == "struct {}" || typ == "struct{}":
		return typ, true
	default:
		slashnum := 0
		slash := -1
		for i, ch := range typ {
			if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' && ch != '.' && ch != '/' && ch != '@' && ch != '%' {
				return "", false
			}
			if ch == '/' {
				slash = i
				slashnum++
			}
		}
		if slashnum <= 1 || slash < 0 {
			return typ, true
		}
		return typ[slash+1:], true
	}
}
