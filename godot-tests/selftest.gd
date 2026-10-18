extends SceneTree
## Headless self-test for addons/burrow_play (task 01a0db5f stage 3; two
## player slots added by task 01a0dbd6 stage 3). Run on a
## Godot 4.7 install from a scratch project holding the addon:
##   godot --headless --path <dir> --script res://selftest.gd -- --keynames=<keynames.json>
## keynames.json is `play-host keynames` output: every Godot key name the
## host's KeyboardEvent.code map can emit. Exits 0 on pass, 1 on failure.
##
## It measures the plan's RISK directly: does Input.parse_input_event update
## the RAW state (Input.is_physical_key_pressed, Input.get_joy_axis,
## Input.is_joy_button_pressed), not only InputMap actions?
## Task 01a0fe45 stage 4 adds touch (st/sd): see _touch_suite.

var failures: Array[String] = []


## The addon reads --burrow-play-* and the older --suite-play-* user args;
## the new prefix wins whatever the order. Tags carry both slot meta names.
func _arg_parsing(script: GDScript) -> void:
	var cur: Dictionary = script.parse_user_args(PackedStringArray(["--burrow-play-session=s1", "--burrow-play-port=40320", "--burrow-play-fps=30"]))
	_check(cur == {"session": "s1", "port": "40320", "fps": "30"}, "args: --burrow-play-* parse (%s)" % [cur])
	var old: Dictionary = script.parse_user_args(PackedStringArray(["--suite-play-session=s2", "--suite-play-port=40321", "--suite-play-export=async"]))
	_check(old == {"session": "s2", "port": "40321", "export": "async"}, "args: legacy --suite-play-* parse (%s)" % [old])
	var both: Dictionary = script.parse_user_args(PackedStringArray(["--burrow-play-session=new", "--suite-play-session=old", "--suite-play-port=1", "--burrow-play-port=2", "--keynames=x"]))
	_check(both == {"session": "new", "port": "2"}, "args: both prefixes, --burrow-play-* wins in either order (%s)" % [both])
	var ev := InputEventKey.new()
	script._tag_slot(ev, 3)
	_check(ev.get_meta(&"burrow_play_slot", -1) == 3 and ev.get_meta(&"suite_play_slot", -1) == 3, "slot tag sets burrow_play_slot and suite_play_slot")


func _check(ok: bool, what: String) -> void:
	print(("PASS " if ok else "FAIL ") + what)
	if not ok:
		failures.append(what)


func _initialize() -> void:
	var script: GDScript = load("res://addons/burrow_play/burrow_play.gd")
	_arg_parsing(script)
	var sp: Node = script.new()
	root.add_child(sp)

	var names_path := ""
	for a in OS.get_cmdline_user_args():
		if a.begins_with("--keynames="):
			names_path = a.get_slice("=", 1)
	if names_path != "":
		var names = JSON.parse_string(FileAccess.get_file_as_string(names_path))
		var bad: Array = []
		for n in names:
			if sp.keycode_for(str(n)) == KEY_NONE:
				bad.append(n)
		_check(bad.is_empty(), "all %d host key names resolve via OS.find_keycode_from_string (unresolved: %s)" % [names.size(), bad])

	sp.handle_line('{"t":"key","k":"W","loc":0,"p":true,"e":false}')
	sp.handle_line('{"t":"key","k":"Shift","loc":2,"p":true,"e":false}')
	sp.handle_line('{"t":"jb","d":0,"b":0,"p":true,"v":1}')
	sp.handle_line('{"t":"ja","d":0,"a":1,"v":-0.75}')
	sp.handle_line('{"t":"ja","d":0,"a":5,"v":0.5}')
	Input.flush_buffered_events()
	_check(Input.is_physical_key_pressed(KEY_W), "key W -> Input.is_physical_key_pressed(KEY_W)")
	_check(Input.is_key_pressed(KEY_W), "key W -> Input.is_key_pressed(KEY_W)")
	_check(Input.is_physical_key_pressed(KEY_SHIFT), "right shift -> Input.is_physical_key_pressed(KEY_SHIFT)")
	_check(Input.is_joy_button_pressed(0, JOY_BUTTON_A), "jb A -> Input.is_joy_button_pressed(0, A)")
	_check(is_equal_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_Y), -0.75), "ja left_y -> Input.get_joy_axis = %.3f" % Input.get_joy_axis(0, JOY_AXIS_LEFT_Y))
	_check(is_equal_approx(Input.get_joy_axis(0, JOY_AXIS_TRIGGER_RIGHT), 0.5), "ja trigger_right -> Input.get_joy_axis = %.3f" % Input.get_joy_axis(0, JOY_AXIS_TRIGGER_RIGHT))
	_check(Input.is_action_pressed("ui_accept") == false, "ui_accept not pressed before Enter")
	sp.handle_line('{"t":"key","k":"Enter","loc":0,"p":true,"e":false}')
	Input.flush_buffered_events()
	_check(Input.is_action_pressed("ui_accept"), "key Enter -> InputMap ui_accept (ExampleGame main.gd:20 reads this)")
	# Report, not assert: a pad driven only through parse_input_event is not a
	# registered joypad, so a game that enumerates pads would not see it.
	print("INFO Input.get_connected_joypads() = %s" % [Input.get_connected_joypads()])

	sp.handle_line('{"t":"release_all"}')
	Input.flush_buffered_events()
	_check(not Input.is_physical_key_pressed(KEY_W) and not Input.is_physical_key_pressed(KEY_ENTER), "release_all releases keys")
	_check(not Input.is_joy_button_pressed(0, JOY_BUTTON_A), "release_all releases buttons")
	_check(is_zero_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_Y)), "release_all zeroes axes")

	_two_devices(sp)


## Task 01a0dbd6: two player slots. The host sends d = slot on every line.
## A game tells two keyboard players apart only in _input (key polling is
## global), and two pads by device in get_joy_axis / is_joy_button_pressed.
## Measured on Godot 4.7.2: events parsed during _initialize never reach
## _input (the tree is not processing yet), so the lines are sent on the
## first frame and read two frames later.
var _rec: Node
var _sp: Node
var _frames := 0
var _kbd := 0


func _two_devices(sp: Node) -> void:
	_sp = sp
	_kbd = InputEventKey.new().device
	print("INFO InputEventKey default device = %d" % _kbd)
	var gd := GDScript.new()
	gd.source_code = "extends Node\nvar got: Array = []\nfunc _input(e: InputEvent) -> void:\n\tif e is InputEventKey or e is InputEventJoypadButton or e is InputEventJoypadMotion:\n\t\tgot.append([e.get_class(), e.device, e.get_meta(&\"burrow_play_slot\", -1), e.is_action(&\"ui_accept\")])\n"
	gd.reload()
	_rec = gd.new()
	root.add_child(_rec)


func _process(_delta: float) -> bool:
	if _touch_started:
		return false
	_frames += 1
	if _frames == 1:
		_sp.handle_line('{"t":"key","d":0,"k":"D","loc":0,"p":true,"e":false}')
		_sp.handle_line('{"t":"key","d":1,"k":"W","loc":0,"p":true,"e":false}')
		_sp.handle_line('{"t":"key","d":0,"k":"Enter","loc":0,"p":true,"e":false}')
		_sp.handle_line('{"t":"key","d":1,"k":"Enter","loc":0,"p":true,"e":false}')
		_sp.handle_line('{"t":"jb","d":0,"b":0,"p":true,"v":1}')
		_sp.handle_line('{"t":"jb","d":1,"b":1,"p":true,"v":1}')
		_sp.handle_line('{"t":"ja","d":0,"a":0,"v":0.5}')
		_sp.handle_line('{"t":"ja","d":1,"a":1,"v":-0.5}')
		return false
	if _frames < 4:
		return false
	var got: Array = _rec.got
	print("INFO _input saw [class, device, meta slot, is ui_accept]: %s" % [got])
	var seen := {}
	for g in got:
		seen["%s:%d:%d" % [g[0], g[1], g[2]]] = true
	_check(seen.has("InputEventKey:%d:0" % _kbd) and seen.has("InputEventKey:%d:1" % (_kbd + 1)), "keys from slots 0 and 1 reach _input on devices %d and %d, meta slot 0 and 1" % [_kbd, _kbd + 1])
	_check(seen.has("InputEventJoypadButton:0:0") and seen.has("InputEventJoypadButton:1:1"), "pad buttons from slots 0 and 1 reach _input with event.device 0 and 1")
	_check(seen.has("InputEventJoypadMotion:0:0") and seen.has("InputEventJoypadMotion:1:1"), "pad axes from slots 0 and 1 reach _input with event.device 0 and 1")
	var enter0 := false
	var enter1 := true
	for g in got:
		if g[0] == "InputEventKey" and g[3]:
			if g[2] == 0:
				enter0 = true
		if g[0] == "InputEventKey" and g[2] == 1 and g[3]:
			enter1 = false
	_check(enter0, "slot 0's Enter is ui_accept (the built-in action is bound to the default key device)")
	print("INFO slot 1's Enter matched ui_accept: %s (built-in ui_* are bound to device %d; an All Devices action matches every slot)" % [not enter1, _kbd])
	_check(is_equal_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_X), 0.5) and is_equal_approx(Input.get_joy_axis(1, JOY_AXIS_LEFT_Y), -0.5) and is_zero_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_Y)), "pad axes are per device when polled (get_joy_axis(d, ...))")
	_check(Input.is_joy_button_pressed(0, JOY_BUTTON_A) and not Input.is_joy_button_pressed(0, JOY_BUTTON_B) and Input.is_joy_button_pressed(1, JOY_BUTTON_B), "pad buttons are per device when polled")
	# Releasing slot 1 leaves slot 0 held.
	_sp.handle_line('{"t":"release","d":1}')
	Input.flush_buffered_events()
	_check(Input.is_joy_button_pressed(0, JOY_BUTTON_A) and not Input.is_joy_button_pressed(1, JOY_BUTTON_B), "release d:1 releases only device 1's buttons")
	_check(is_equal_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_X), 0.5) and is_zero_approx(Input.get_joy_axis(1, JOY_AXIS_LEFT_Y)), "release d:1 zeroes only device 1's axes")
	_check(Input.is_physical_key_pressed(KEY_D) and not Input.is_physical_key_pressed(KEY_W), "release d:1 lifts slot 1's W and keeps slot 0's D")
	# Recorded, not asserted: key polling has no device, so if both slots hold
	# the same key, releasing one lifts it for both.
	_sp.handle_line('{"t":"key","d":1,"k":"D","loc":0,"p":true,"e":false}')
	_sp.handle_line('{"t":"release","d":1}')
	Input.flush_buffered_events()
	print("INFO slot 0 and slot 1 both held D, slot 1 released: Input.is_physical_key_pressed(KEY_D) = %s (polling is global)" % Input.is_physical_key_pressed(KEY_D))
	_sp.handle_line('{"t":"release_all"}')
	Input.flush_buffered_events()
	_check(not Input.is_joy_button_pressed(0, JOY_BUTTON_A) and is_zero_approx(Input.get_joy_axis(0, JOY_AXIS_LEFT_X)) and not Input.is_physical_key_pressed(KEY_D), "release_all still releases every device")

	_touch_started = true
	_touch_suite()
	return false


## ---- Touch (task 01a0fe45 stage 4) ----
##
## The host sends st/sd with x, y normalised to the exported frame (the root
## viewport texture). In every stretch configuration below the test computes
## the expected normalised point for a canvas point c independently of the
## addon, as c / root.get_visible_rect().size (the canvas fills the texture;
## black bars are outside it), and asserts the texture size Godot's own
## Window::_update_viewport_size formula predicts, by hand, for that row.
## Each row then checks, through the real Viewport GUI path:
##   positive control: a direct InputEventMouseButton at the Button centre
##     (window coords from root.get_final_transform()) fires pressed once;
##   tap: st down/up at the Button centre fires pressed EXACTLY once;
##   canceled: st down, st up c:true fires it 0 times;
##   _input sees the touch at the expected canvas point (within 1 px);
##   an sd drag inside the ScrollContainer changes scroll_vertical.
## Rows (mode, aspect, base content size, window size):
const ROWS := [
	["disabled portrait", Window.CONTENT_SCALE_MODE_DISABLED, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(0, 0), Vector2i(450, 1000), Vector2i(450, 1000)],
	["disabled landscape", Window.CONTENT_SCALE_MODE_DISABLED, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(0, 0), Vector2i(1600, 900), Vector2i(1600, 900)],
	# Survival Game at 3c921947 project.godot: viewport 450x1000, window
	# override 450x1000, stretch mode canvas_items, aspect unset (keep).
	["survival-game@3c92194 canvas_items keep 450x1000 in 450x1000", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(450, 1000), Vector2i(450, 1000), Vector2i(450, 1000)],
	["canvas_items keep 450x1000 in landscape 1600x900 (bars)", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(450, 1000), Vector2i(1600, 900), Vector2i(405, 900)],
	["canvas_items keep 1600x900 in landscape 1600x900", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(1600, 900), Vector2i(1600, 900), Vector2i(1600, 900)],
	["canvas_items keep 1600x900 in portrait 900x1600 (bars)", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(1600, 900), Vector2i(900, 1600), Vector2i(900, 506)],
	["canvas_items expand 450x1000 in portrait 900x1600", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_EXPAND, Vector2i(450, 1000), Vector2i(900, 1600), Vector2i(900, 1600)],
	["canvas_items expand 450x1000 in landscape 1600x900", Window.CONTENT_SCALE_MODE_CANVAS_ITEMS, Window.CONTENT_SCALE_ASPECT_EXPAND, Vector2i(450, 1000), Vector2i(1600, 900), Vector2i(1600, 900)],
	["viewport keep 450x1000 in portrait 900x1600 (bars)", Window.CONTENT_SCALE_MODE_VIEWPORT, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(450, 1000), Vector2i(900, 1600), Vector2i(450, 1000)],
	["viewport keep 450x1000 in landscape 1600x900 (bars)", Window.CONTENT_SCALE_MODE_VIEWPORT, Window.CONTENT_SCALE_ASPECT_KEEP, Vector2i(450, 1000), Vector2i(1600, 900), Vector2i(450, 1000)],
]

var _touch_rec: Node
var _touch_started := false
var _presses := 0


func _frames_wait(n: int) -> void:
	for _i in n:
		await process_frame


func _flush() -> void:
	Input.flush_buffered_events()
	await _frames_wait(2)


func _on_pressed() -> void:
	_presses += 1


func _touch_suite() -> void:
	var gd := GDScript.new()
	gd.source_code = "extends Node\nvar got: Array = []\nfunc _input(e: InputEvent) -> void:\n\tif e is InputEventScreenTouch:\n\t\tgot.append([\"st\", e.index, e.pressed, e.canceled, e.position, e.get_meta(&\"suite_play_slot\", -1), e.device])\n\telif e is InputEventScreenDrag:\n\t\tgot.append([\"sd\", e.index, e.position, e.relative, e.get_meta(&\"suite_play_slot\", -1), e.velocity])\n"
	gd.reload()
	_touch_rec = gd.new()
	root.add_child(_touch_rec)
	print("INFO touch: emulate_mouse_from_touch=%s emulate_touch_from_mouse=%s is_touchscreen_available=%s (before any touch line)" % [Input.is_emulating_mouse_from_touch(), Input.is_emulating_touch_from_mouse(), DisplayServer.is_touchscreen_available()])
	var first := true
	for row in ROWS:
		await _touch_row(row, first)
		first = false
	await _touch_release()
	print("SELFTEST %s (%d failures)" % ["PASS" if failures.is_empty() else "FAIL", failures.size()])
	quit(0 if failures.is_empty() else 1)


func _norm(c: Vector2) -> Vector2:
	return c / root.get_visible_rect().size


func _st(d: int, i: int, p: bool, c: bool, n: Vector2) -> void:
	_sp.handle_line(JSON.stringify({"t": "st", "d": d, "i": i, "p": p, "c": c, "x": n.x, "y": n.y}))


func _sd(d: int, i: int, n: Vector2) -> void:
	_sp.handle_line(JSON.stringify({"t": "sd", "d": d, "i": i, "x": n.x, "y": n.y}))


func _touch_row(row: Array, first: bool) -> void:
	var name: String = row[0]
	root.content_scale_mode = row[1]
	root.content_scale_aspect = row[2]
	root.content_scale_size = row[3]
	root.size = row[4]
	var holder := Control.new()
	holder.mouse_filter = Control.MOUSE_FILTER_IGNORE
	root.add_child(holder)
	await _frames_wait(2)
	var vis := root.get_visible_rect().size
	holder.position = Vector2.ZERO
	holder.size = vis
	var button := Button.new()
	button.text = "tap"
	button.position = (vis * Vector2(0.1, 0.05)).floor()
	button.size = (vis * Vector2(0.4, 0.08)).floor()
	button.pressed.connect(_on_pressed)
	holder.add_child(button)
	var scroll := ScrollContainer.new()
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	scroll.position = (vis * Vector2(0.1, 0.3)).floor()
	scroll.size = (vis * Vector2(0.8, 0.5)).floor()
	holder.add_child(scroll)
	var content := Control.new()
	content.mouse_filter = Control.MOUSE_FILTER_PASS
	content.custom_minimum_size = (vis * Vector2(0.7, 3.0)).floor()
	scroll.add_child(content)
	await _frames_wait(3)
	var fs: Vector2 = _sp.frame_size()
	var want_fs: Vector2i = row[5]
	var img := root.get_texture().get_image()
	var img_size := Vector2i(-1, -1) if img == null or img.is_empty() else img.get_size()
	print("INFO [%s] window=%s visible=%s frame_size=%s texture.get_size=%s image=%s final=%s stretch=%s" % [name, root.size, vis, fs, root.get_texture().get_size(), img_size, root.get_final_transform(), root.get_stretch_transform()])
	_check(Vector2i(fs.round()) == want_fs, "[%s] frame_size() is %s, the render size Godot's stretch formula predicts by hand (got %s)" % [name, want_fs, fs])
	if img_size.x >= 0:
		_check(img_size == want_fs, "[%s] the exported image (root get_image) is %s (got %s)" % [name, want_fs, img_size])
	else:
		print("INFO [%s] no exported image under this renderer (headless dummy); image size unmeasured here" % name)

	var centre := button.get_global_rect().get_center()
	# Positive control: a direct mouse click in window coordinates.
	var wpos := root.get_final_transform() * centre
	_presses = 0
	var mm := InputEventMouseMotion.new()
	mm.position = wpos
	mm.global_position = wpos
	Input.parse_input_event(mm)
	for down in [true, false]:
		var mb := InputEventMouseButton.new()
		mb.button_index = MOUSE_BUTTON_LEFT
		mb.pressed = down
		mb.button_mask = MOUSE_BUTTON_MASK_LEFT if down else 0
		mb.position = wpos
		mb.global_position = wpos
		Input.parse_input_event(mb)
		await _flush()
	_check(_presses == 1, "[%s] POSITIVE CONTROL: a direct InputEventMouseButton at the Button centre fires pressed once (fired %d)" % [name, _presses])

	if first:
		# Without emulate_touch_from_mouse the drag must NOT scroll: shows the
		# addon's switch is what makes ScrollContainer touch-scroll work.
		_sp.touchscreen_on_touch = false
		Input.emulate_touch_from_mouse = false
		var moved0: float = await _drag(scroll)
		_check(is_zero_approx(moved0), "[%s] with is_touchscreen_available()=false an sd drag does not scroll (moved %.1f): ScrollContainer needs it" % [name, moved0])
		_sp.touchscreen_on_touch = true

	# Tap.
	_presses = 0
	_touch_rec.got.clear()
	_st(0, 0, true, false, _norm(centre))
	await _flush()
	_st(0, 0, false, false, _norm(centre))
	await _flush()
	_check(_presses == 1, "[%s] tap (st down/up at the Button centre) fires pressed EXACTLY once (fired %d)" % [name, _presses])
	var seen_at: Variant = null
	for g in _touch_rec.got:
		if g[0] == "st" and g[1] == 0 and g[2]:
			seen_at = g[4]
	_check(seen_at != null and (seen_at as Vector2).distance_to(centre) <= 1.0, "[%s] _input sees the touch at canvas %s (expected %s)" % [name, seen_at, centre])
	_check(not _touch_rec.got.is_empty() and _touch_rec.got[0][5] == 0, "[%s] touch carries the legacy meta suite_play_slot 0 too" % name)

	# Canceled touch.
	_presses = 0
	_st(0, 0, true, false, _norm(centre))
	await _flush()
	_st(0, 0, false, true, _norm(centre))
	await _flush()
	_check(_presses == 0, "[%s] canceled touch (c:true) fires pressed 0 times (fired %d)" % [name, _presses])

	# Two players: slot 0 holds touch 0 on empty space (so it owns the
	# emulated mouse), slot 1 taps the Button with touch 10. Godot 4.7.2
	# BaseButton also reads non-emulated ScreenTouch (base_button.cpp:66), so
	# measure what the second player's tap does.
	_presses = 0
	_st(0, 0, true, false, Vector2(0.95, 0.97))
	await _flush()
	_st(1, 10, true, false, _norm(centre))
	await _flush()
	_st(1, 10, false, false, _norm(centre))
	await _flush()
	var p2 := _presses
	_st(0, 0, false, false, Vector2(0.95, 0.97))
	await _flush()
	_check(p2 == 1 and _presses == 1, "[%s] second player's tap (index 10) while slot 0 holds touch 0 elsewhere fires pressed exactly once (fired %d; %d after slot 0 lifts)" % [name, p2, _presses])

	var moved: float = await _drag(scroll)
	_check(moved > 0.0, "[%s] an sd drag inside the ScrollContainer changes scroll_vertical (by %.1f)" % [name, moved])
	print("INFO [%s] is_touchscreen_available=%s held after row=%s" % [name, DisplayServer.is_touchscreen_available(), _sp.held_touches()])
	holder.queue_free()
	await _frames_wait(2)


## Press in the ScrollContainer's centre, drag up by 30% of its height in 6
## moves, lift. Returns how far scroll_vertical moved.
func _drag(scroll: ScrollContainer) -> float:
	var before := float(scroll.scroll_vertical)
	var c := scroll.get_global_rect().get_center()
	var h := scroll.size.y
	_st(0, 1, true, false, _norm(c))
	await _flush()
	for k in range(1, 7):
		_sd(0, 1, _norm(c - Vector2(0, h * 0.05 * k)))
		await _flush()
	var after := float(scroll.scroll_vertical)
	_st(0, 1, false, false, _norm(c - Vector2(0, h * 0.3)))
	await _flush()
	return after - before


func _touch_release() -> void:
	_touch_rec.got.clear()
	_st(0, 0, true, false, Vector2(0.9, 0.95))
	_st(1, 10, true, false, Vector2(0.8, 0.95))
	await _flush()
	var held: Array = _sp.held_touches()
	held.sort()
	_check(held == [0, 10], "two slots hold touches 0 (slot 0) and 10 (slot 1): %s" % [held])
	_touch_rec.got.clear()
	_sp.release_device(1)
	await _flush()
	held = _sp.held_touches()
	_check(held == [0], "release_device(1) lifts slot 1's touch (index 10) and not slot 0's (index 0): held %s" % [held])
	var lifted := []
	for g in _touch_rec.got:
		if g[0] == "st" and not g[2]:
			lifted.append([g[1], g[3], g[5]])
	_check(lifted == [[10, true, 1]], "release_device(1) sends one canceled touch, index 10, meta slot 1: %s" % [lifted])
	_touch_rec.got.clear()
	_sp.release_all()
	await _flush()
	_check(_sp.held_touches().is_empty(), "release_all leaves no held touch: %s" % [_sp.held_touches()])
	lifted = []
	for g in _touch_rec.got:
		if g[0] == "st" and not g[2]:
			lifted.append([g[1], g[3], g[5]])
	_check(lifted == [[0, true, 0]], "release_all sends a canceled touch for index 0, meta slot 0: %s" % [lifted])
