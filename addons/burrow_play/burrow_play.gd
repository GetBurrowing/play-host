extends Node
## Burrow play addon (autoload `BurrowPlay`, task 01a0db5f). Inert unless Godot
## was started with the user arg --burrow-play-session=<id> (or the older
## --suite-play-session=<id>) after "--".
##
## Listens on 127.0.0.1:<--burrow-play-port> for exactly one connection from
## the play host, which carries:
##   host -> game: newline-delimited JSON (see internal/input in play-host):
##     {"t":"key","d":0,"k":"W","loc":0,"p":true,"e":false}  k = Godot key name, device d
##     {"t":"jb","d":0,"b":0,"p":true,"v":1}                joypad button, device d
##     {"t":"ja","d":0,"a":0,"v":-0.5}                     joypad axis, device d
##     {"t":"st","d":0,"i":0,"p":true,"c":false,"x":0.42,"y":0.77}  InputEventScreenTouch
##     {"t":"sd","d":0,"i":0,"x":0.45,"y":0.70}                     InputEventScreenDrag
##     {"t":"release","d":0}  releases what device d holds (keys, pads, touches)
##     {"t":"release_all"}  {"t":"probe","seq":N}  {"t":"export","on":true}
## d is the player SLOT the host bound the input to (P1 = 0), never a value
## the browser chose. A key line without d is slot 0 (a v1 host).
## Touch (task 01a0fe45): i is the Godot touch index, slot*10 + browser id, so
## two players never share an index. x, y are in [0, 1] of the EXPORTED FRAME
## (the root viewport's image that _on_frame_post_draw sends) and are
## mapped to root Window coordinates (frame_to_window), which
## parse_input_event then takes through the stretch transform like an OS event.
##
## Device ids (task 01a0dbd6, measured on Godot 4.7.2): a key event's default
## device is 16 (the keyboard id), and the built-in ui_* actions are bound to
## device 16, so a key event with device 0 does NOT match ui_accept. So keys
## from slot d carry device <default> + d (P1 keeps the default, exactly what
## a v1 key had, so every InputMap action still fires for P1), and pads carry
## device d. Every event also carries meta "burrow_play_slot" = d (and "suite_play_slot", its older name).
##   game -> host: frames while export is on (frame path C, stage 1).
## Audio record (little-endian): "SPA1" (version 1), u32 sample rate (48000),
## u32 channels (2), u32 format (1 = interleaved f32le), u32 sample frames,
## u32 payload length, bytes. Source is AudioEffectCapture on Master only;
## no microphone or desktop-loopback API is used.
## Frame record (little-endian): "SPF1", u32 width, u32 height, u32 kind
## (1 = Image.FORMAT_RGBA8 from get_image, 2 = RD texture bytes), u32 length, bytes.
##
## All input goes through Input.parse_input_event: no ViGEm, no virtual
## device driver. Mapping and edge-diffing are done by the host (Go, unit
## tested); this file only resolves key names and builds the InputEvents.
##
## Probe marker: a 4x4 grid of 16x16 px cells in the top-left corner, row-major,
## MSB first, value = seq << 4 | check(seq), check = (s ^ s>>4 ^ s>>8 ^ 0xA) & 0xF.
## Drawn for 2 frames per probe, black otherwise. Pinned by internal/probe in Go.
## A multi-player host forwards seq = slot << 10 | (seq & 0x3FF), so the top 2
## bits name the slot; this file draws whatever seq it is given.
##
## For games (task 01a0dbd6, recorded for ExampleGame): the slot of any event is
## event.get_meta("burrow_play_slot", 0); for a key it is also
## event.device - 16, for a pad event.device. KEY POLLING has no device:
## Input.is_physical_key_pressed / is_key_pressed / is_action_pressed are
## global, so two keyboard players are distinct ONLY in _input /
## _unhandled_input. An action bound to device 16 (the ui_* defaults) fires
## for P1's keys only; an action set to "All Devices" fires for every slot.
## Pads are distinct by polling too, through Input.get_joy_axis(d, ...) and
## is_joy_button_pressed(d, ...).
##
## Touch, for games (task 01a0fe45): the addon builds InputEventScreenTouch and
## InputEventScreenDrag ONLY, never a mouse event. Controls activate through
## Godot's input_devices/pointing/emulate_mouse_from_touch (default on), which
## emulates the mouse from ONE touch at a time: with two simultaneous players
## only the first touch down drives the emulated mouse, so only it drives
## mouse-only Controls (ScrollContainer drag, sliders, text fields). The second
## player's touch is a ScreenTouch only. BaseButton (Button, CheckBox, ...)
## also reads a raw ScreenTouch (Godot 4.7.2 base_button.cpp:66), so a second
## player's tap DOES press a Button, once (measured in godot-tests/selftest.gd);
## a first player's tap also presses it once, the emulated mouse release firing
## it and the touch release then finding no press attempt. A game that turns
## emulate_mouse_from_touch off gets no emulated mouse and must handle
## ScreenTouch itself. The slot is event.get_meta("burrow_play_slot") or
## event.index / 10.
## On the first touch line the addon sets Input.emulate_touch_from_mouse, the
## only thing that makes DisplayServer.is_touchscreen_available() true on a
## Windows host without a digitizer (Godot 4.7.2 display_server.cpp:583).
## ScrollContainer drag-scrolls, and TouchScreenButton shows, only when that is
## true. The play host has no physical mouse, and an emulated mouse event is
## never turned back into a touch (input.cpp:850, !p_is_emulated), so this adds
## no second activation.

const CELLS := 4
const CELL_PX := 16
const PROBE_FRAMES := 2
const MAX_SEQ := 4095
const SLOT_META := &"burrow_play_slot"
## Older games read the slot under its previous name; every event carries both.
const LEGACY_SLOT_META := &"suite_play_slot"
## User-arg prefixes. A play host emits both; an older host emits only the
## legacy one. When both spell the same option, the new prefix wins.
const ARG_PREFIX := "--burrow-play-"
const LEGACY_ARG_PREFIX := "--suite-play-"
const ARG_NAMES := ["session", "port", "export", "fps"]
const AUDIO_RATE := 48000
const AUDIO_CHANNELS := 2
const AUDIO_FORMAT_F32LE := 1
const AUDIO_CHUNK_FRAMES := 960  # 20 ms
const AUDIO_MAX_BACKLOG := 4800  # 100 ms
const AUDIO_MAX_CHUNKS_PER_TICK := 5

var session_id := ""
var port := 0
## "image" | "async" | "none". The spike passed --burrow-play-export and
## exported always; the play host leaves it unset and switches export on and
## off with {"t":"export"} so an unwatched game spends no GPU on readback.
var export_mode := "image"
var export_forced := false
var exporting := false
var _server := TCPServer.new()
var _peer: StreamPeerTCP
var _rx := PackedByteArray()
var _layer: CanvasLayer
var _cells: Array[ColorRect] = []
var _probe_left := 0
var _rd_tex := RID()
var _async_inflight := 0
var frames_sent := 0
var probes_seen := 0
var audio_chunks_sent := 0
var audio_frames_dropped := 0
var _capture: AudioEffectCapture
var _audio_mix_rate := AUDIO_RATE
# Streaming rational resampler state. Source frames are accumulated until the
# 48 kHz output clock advances; downsampling averages each covered source
# window, while an input rate below 48 kHz repeats the current window sample.
var _audio_resample_clock := 0
var _audio_resample_sum := Vector2.ZERO
var _audio_resample_count := 0
var _keycodes := {}  # name -> Key
var _held_keys := {}  # "d:keycode:loc" -> [keycode, loc, d]
var _held_buttons := {}  # "d:b" -> [d, b]
var _held_axes := {}  # "d:a" -> [d, a]
var _held_touches := {}  # index -> [d, window position, Time.get_ticks_usec()]
var _checked_key := false
var _checked_axis := false


## Reads the addon's options from Godot's user args (the ones after "--").
## Accepts both --burrow-play-<name>= and --suite-play-<name>=; the
## --burrow-play- spelling wins whatever the order. Returns only the names
## that were present, as strings.
static func parse_user_args(args: PackedStringArray) -> Dictionary:
	var legacy := {}
	var current := {}
	for a in args:
		for n in ARG_NAMES:
			if a.begins_with(ARG_PREFIX + n + "="):
				current[n] = a.substr((ARG_PREFIX + n + "=").length())
			elif a.begins_with(LEGACY_ARG_PREFIX + n + "="):
				legacy[n] = a.substr((LEGACY_ARG_PREFIX + n + "=").length())
	legacy.merge(current, true)
	return legacy


static func _tag_slot(ev: InputEvent, slot: int) -> void:
	ev.set_meta(SLOT_META, slot)
	ev.set_meta(LEGACY_SLOT_META, slot)


func _ready() -> void:
	var opts := parse_user_args(OS.get_cmdline_user_args())
	if opts.has("session"):
		session_id = opts["session"]
	if opts.has("port"):
		port = int(opts["port"])
	if opts.has("export"):
		export_mode = opts["export"]
		export_forced = export_mode != "none"
	if opts.has("fps"):
		Engine.max_fps = int(opts["fps"])
	exporting = export_forced
	if session_id == "" or port <= 0:
		set_process(false)
		return
	if Engine.max_fps == 0:
		Engine.max_fps = 60
	process_mode = Node.PROCESS_MODE_ALWAYS
	var err := _server.listen(port, "127.0.0.1")
	print("burrow_play: session=%s port=%d export=%s listen=%s" % [session_id, port, export_mode, error_string(err)])
	_build_marker()
	_setup_audio_capture()
	RenderingServer.frame_post_draw.connect(_on_frame_post_draw)
func _build_marker() -> void:
	_layer = CanvasLayer.new()
	_layer.layer = 128
	add_child(_layer)
	for i in CELLS * CELLS:
		var r := ColorRect.new()
		r.color = Color.BLACK
		r.position = Vector2((i % CELLS) * CELL_PX, (i / CELLS) * CELL_PX)
		r.size = Vector2(CELL_PX, CELL_PX)
		r.mouse_filter = Control.MOUSE_FILTER_IGNORE
		_layer.add_child(r)
		_cells.append(r)


static func check(seq: int) -> int:
	var s := seq & MAX_SEQ
	return (s ^ (s >> 4) ^ (s >> 8) ^ 0xA) & 0xF


func show_probe(seq: int) -> void:
	var v := ((seq & MAX_SEQ) << 4) | check(seq)
	for i in CELLS * CELLS:
		_cells[i].color = Color.WHITE if (v >> (15 - i)) & 1 == 1 else Color.BLACK
	_probe_left = PROBE_FRAMES
	probes_seen += 1


func _clear_probe() -> void:
	for r in _cells:
		r.color = Color.BLACK


func _process(_delta: float) -> void:
	if _server.is_connection_available():
		if _peer != null:
			_peer.disconnect_from_host()
		_peer = _server.take_connection()
		_peer.set_no_delay(true)
		print("burrow_play: host connected")
	if _peer == null:
		if _capture != null:
			_capture.clear_buffer()
		return
	_peer.poll()
	if _peer.get_status() != StreamPeerTCP.STATUS_CONNECTED:
		print("burrow_play: host link closed; releasing input")
		release_all()
		_peer = null
		exporting = export_forced
		return
	var n := _peer.get_available_bytes()
	if n > 0:
		var got := _peer.get_partial_data(n)
		if got[0] == OK:
			_rx.append_array(got[1])
	while true:
		var nl := _rx.find(10)
		if nl < 0:
			break
		var line := _rx.slice(0, nl).get_string_from_utf8()
		_rx = _rx.slice(nl + 1)
		handle_line(line)
	_drain_audio()

## One host line. Public so the headless self-test can drive it.
func handle_line(line: String) -> void:
	var msg = JSON.parse_string(line)
	if typeof(msg) != TYPE_DICTIONARY:
		return
	match str(msg.get("t", "")):
		"probe":
			show_probe(int(msg.get("seq", 0)))
		"key":
			_key(str(msg.get("k", "")), int(msg.get("loc", 0)), bool(msg.get("p", false)), bool(msg.get("e", false)), int(msg.get("d", 0)))
		"jb":
			_joy_button(int(msg.get("d", 0)), int(msg.get("b", 0)), bool(msg.get("p", false)), float(msg.get("v", 0.0)))
		"ja":
			_joy_axis(int(msg.get("d", 0)), int(msg.get("a", 0)), float(msg.get("v", 0.0)))
		"st":
			_touch(int(msg.get("d", 0)), int(msg.get("i", 0)), bool(msg.get("p", false)), bool(msg.get("c", false)), float(msg.get("x", 0.0)), float(msg.get("y", 0.0)))
		"sd":
			_drag(int(msg.get("d", 0)), int(msg.get("i", 0)), float(msg.get("x", 0.0)), float(msg.get("y", 0.0)))
		"release":
			release_device(int(msg.get("d", 0)))
		"release_all":
			release_all()
		"export":
			exporting = bool(msg.get("on", false)) or export_forced
			if _capture != null:
				_capture.clear_buffer()
			print("burrow_play: export %s" % exporting)


## Key name (as OS.find_keycode_from_string reads it) -> Key, cached. 0 if unknown.
## The host sends platform-neutral names; Godot spells META per platform
## ("Windows" on Windows, "Command" on macOS), so it is special-cased here.
## Measured on host-a: without this, "Meta" resolved to KEY_NONE.
func keycode_for(name: String) -> int:
	if not _keycodes.has(name):
		match name:
			"Meta":
				_keycodes[name] = KEY_META
			"Alt":
				_keycodes[name] = KEY_ALT
			_:
				_keycodes[name] = OS.find_keycode_from_string(name)
	return int(_keycodes[name])


func _key(name: String, loc: int, pressed: bool, echo: bool, device: int = 0) -> void:
	var code := keycode_for(name)
	if code == KEY_NONE:
		return
	var ev := _key_event(code, loc, device)
	ev.pressed = pressed
	ev.echo = echo
	var id := "%d:%d:%d" % [device, code, loc]
	if pressed:
		_held_keys[id] = [code, loc, device]
	else:
		_held_keys.erase(id)
	Input.parse_input_event(ev)
	if not _checked_key:
		_checked_key = true
		Input.flush_buffered_events()
		print("burrow_play: first key %s pressed=%s -> Input.is_physical_key_pressed=%s" % [name, pressed, Input.is_physical_key_pressed(code)])


## A key event for slot `slot`: the default key device + slot, slot in meta.
static func _key_event(code: int, loc: int, slot: int) -> InputEventKey:
	var ev := InputEventKey.new()
	ev.device += slot
	ev.keycode = code
	ev.physical_keycode = code
	ev.location = loc
	_tag_slot(ev, slot)
	return ev


func _joy_button(device: int, button: int, pressed: bool, value: float) -> void:
	var ev := InputEventJoypadButton.new()
	ev.device = device
	_tag_slot(ev, device)
	ev.button_index = button
	ev.pressed = pressed
	ev.pressure = value
	var id := "%d:%d" % [device, button]
	if pressed:
		_held_buttons[id] = [device, button]
	else:
		_held_buttons.erase(id)
	Input.parse_input_event(ev)


func _joy_axis(device: int, axis: int, value: float) -> void:
	var ev := InputEventJoypadMotion.new()
	ev.device = device
	_tag_slot(ev, device)
	ev.axis = axis
	ev.axis_value = value
	var id := "%d:%d" % [device, axis]
	if absf(value) > 0.0:
		_held_axes[id] = [device, axis]
	else:
		_held_axes.erase(id)
	Input.parse_input_event(ev)
	if not _checked_axis:
		_checked_axis = true
		Input.flush_buffered_events()
		print("burrow_play: first axis d%d a%d v=%.3f -> Input.get_joy_axis=%.3f" % [device, axis, value, Input.get_joy_axis(device, axis)])


## Size in pixels of the exported frame: the root viewport's render target,
## which get_image() reads. It is the window size (stretch disabled), the
## scaled content box inside any black bars (canvas_items) or the base size
## (viewport): the visible canvas rect times the stretch scale.
## NOT get_viewport().get_texture().get_size(): for a root Window that applies
## the stretch scale a second time (Godot 4.7.2 viewport.cpp:174; measured
## 365x810 for a 405x900 canvas_items render).
func frame_size() -> Vector2:
	var vp := get_viewport()
	return vp.get_visible_rect().size * vp.get_stretch_transform().get_scale()


## Normalised exported-frame coordinates -> root Window event coordinates.
## A Window maps render pixels to window pixels with its window_transform
## (bar margin, plus the scale in viewport mode), and
## get_final_transform() = window_transform * stretch * global canvas, so
## window_transform = final * (stretch * global canvas)^-1.
## parse_input_event then takes the event back through final^-1, like an OS
## event, so a Control sees it in canvas coordinates.
func frame_to_window(x: float, y: float) -> Vector2:
	var vp := get_viewport()
	var render := vp.get_stretch_transform() * vp.global_canvas_transform
	return vp.get_final_transform() * render.affine_inverse() * (Vector2(x, y) * frame_size())


## Selftest hook: false skips turning emulate_touch_from_mouse on, so the
## selftest can measure what a touch drag does without it.
var touchscreen_on_touch := true


func _touch_ready() -> void:
	if touchscreen_on_touch and not Input.is_emulating_touch_from_mouse():
		Input.emulate_touch_from_mouse = true
		print("burrow_play: first touch; emulate_touch_from_mouse on (is_touchscreen_available=%s), emulate_mouse_from_touch=%s" % [DisplayServer.is_touchscreen_available(), Input.is_emulating_mouse_from_touch()])


static func _touch_event(d: int, index: int, pressed: bool, canceled: bool, pos: Vector2) -> InputEventScreenTouch:
	var ev := InputEventScreenTouch.new()
	_tag_slot(ev, d)
	ev.index = index
	ev.pressed = pressed
	ev.canceled = canceled
	ev.position = pos
	return ev


func _touch(d: int, index: int, pressed: bool, canceled: bool, x: float, y: float) -> void:
	_touch_ready()
	var pos := frame_to_window(x, y)
	if pressed:
		_held_touches[index] = [d, pos, Time.get_ticks_usec()]
	else:
		_held_touches.erase(index)
	Input.parse_input_event(_touch_event(d, index, pressed, canceled and not pressed, pos))


## A drag on an index that is not held is not an edge (Godot never sends one).
func _drag(d: int, index: int, x: float, y: float) -> void:
	if not _held_touches.has(index):
		return
	var pos := frame_to_window(x, y)
	var h: Array = _held_touches[index]
	var now := Time.get_ticks_usec()
	var dt := maxf(float(now - int(h[2])) / 1000000.0, 0.001)
	var ev := InputEventScreenDrag.new()
	_tag_slot(ev, d)
	ev.index = index
	ev.position = pos
	ev.relative = pos - Vector2(h[1])
	ev.screen_relative = ev.relative
	# Input recomputes velocity from its own track of relative; this is the
	# value a reader of the raw event sees before that.
	ev.velocity = ev.relative / dt
	ev.screen_velocity = ev.velocity
	ev.pressure = 1.0
	_held_touches[index] = [d, pos, now]
	Input.parse_input_event(ev)


## Touch indices this addon holds pressed (selftest reads it).
func held_touches() -> Array:
	return _held_touches.keys()


## Releases every key, button, axis and touch this addon pressed, on every device.
func release_all() -> void:
	_release(-1)


## Releases what device d holds, and nothing another device holds (one
## player leaving, or being taken over, must not lift another's keys).
func release_device(d: int) -> void:
	_release(d)


## device -1 = all.
func _release(device: int) -> void:
	for id in _held_keys.keys():
		var k: Array = _held_keys[id]
		if device >= 0 and k[2] != device:
			continue
		var ev := _key_event(k[0], k[1], k[2])
		ev.pressed = false
		Input.parse_input_event(ev)
		_held_keys.erase(id)
	for id in _held_buttons.keys():
		var b: Array = _held_buttons[id]
		if device >= 0 and b[0] != device:
			continue
		var ev := InputEventJoypadButton.new()
		ev.device = b[0]
		_tag_slot(ev, b[0])
		ev.button_index = b[1]
		ev.pressed = false
		Input.parse_input_event(ev)
		_held_buttons.erase(id)
	for id in _held_axes.keys():
		var a: Array = _held_axes[id]
		if device >= 0 and a[0] != device:
			continue
		var ev := InputEventJoypadMotion.new()
		ev.device = a[0]
		_tag_slot(ev, a[0])
		ev.axis = a[1]
		ev.axis_value = 0.0
		Input.parse_input_event(ev)
		_held_axes.erase(id)
	for index in _held_touches.keys():
		var t: Array = _held_touches[index]
		if device >= 0 and t[0] != device:
			continue
		Input.parse_input_event(_touch_event(t[0], index, false, true, t[1]))
		_held_touches.erase(index)


func _on_frame_post_draw() -> void:
	if _probe_left > 0:
		_probe_left -= 1
		if _probe_left == 0:
			_clear_probe()
	if _peer == null or not exporting:
		return
	match export_mode:
		"image":
			var img := get_viewport().get_texture().get_image()
			if img.get_format() != Image.FORMAT_RGBA8:
				img.convert(Image.FORMAT_RGBA8)
			_send_frame(img.get_width(), img.get_height(), 1, img.get_data())
		"async":
			_request_async()


func _request_async() -> void:
	if _async_inflight >= 2:
		return
	var rd := RenderingServer.get_rendering_device()
	if rd == null:
		return
	if not _rd_tex.is_valid():
		_rd_tex = RenderingServer.texture_get_rd_texture(get_viewport().get_texture().get_rid())
		var fmt := rd.texture_get_format(_rd_tex)
		print("burrow_play: rd texture format=%d %dx%d" % [fmt.format, fmt.width, fmt.height])
	var size := get_viewport().get_texture().get_size()
	_async_inflight += 1
	rd.texture_get_data_async(_rd_tex, 0, func(data: PackedByteArray) -> void:
		_async_inflight -= 1
		_send_frame(int(size.x), int(size.y), 2, data))


func _send_frame(w: int, h: int, kind: int, data: PackedByteArray) -> void:
	if _peer == null:
		return
	var hdr := StreamPeerBuffer.new()
	hdr.put_data("SPF1".to_ascii_buffer())
	hdr.put_u32(w)
	hdr.put_u32(h)
	hdr.put_u32(kind)
	hdr.put_u32(data.size())
	_peer.put_data(hdr.data_array)
	_peer.put_data(data)
	frames_sent += 1


## Installs a pass-through capture effect on the game's Master bus. This sees
## only PCM mixed by this Godot process. AudioEffectCapture does not record an
## OS input/output device and does not mute or otherwise alter the bus.
func _setup_audio_capture() -> void:
	_audio_mix_rate = int(AudioServer.get_mix_rate())
	if _audio_mix_rate <= 0:
		push_error("burrow_play: Master mix rate is invalid: %d" % _audio_mix_rate)
		return
	var master := AudioServer.get_bus_index("Master")
	if master < 0:
		push_error("burrow_play: Master bus not found")
		return
	_capture = AudioEffectCapture.new()
	_capture.buffer_length = 0.2
	AudioServer.add_bus_effect(master, _capture)
	if _audio_mix_rate == AUDIO_RATE:
		print("burrow_play: capturing Master PCM %d Hz stereo f32le" % AUDIO_RATE)
	else:
		print("burrow_play: capturing Master PCM %d Hz -> %d Hz stereo f32le" % [_audio_mix_rate, AUDIO_RATE])


## Drains at most 100 ms per process tick in 20 ms records. If the game or TCP
## path falls behind, old samples are discarded instead of building latency.
func _drain_audio() -> void:
	if _capture == null or _peer == null or not exporting:
		return
	var available := _capture.get_frames_available()
	var max_backlog := ceili(float(AUDIO_MAX_BACKLOG * _audio_mix_rate) / AUDIO_RATE)
	if available > max_backlog:
		var drop := available - max_backlog
		_capture.get_buffer(drop)
		audio_frames_dropped += drop
		available = max_backlog
	var source_chunk := ceili(float(AUDIO_CHUNK_FRAMES * _audio_mix_rate) / AUDIO_RATE)
	var chunks := 0
	while available > 0 and chunks < AUDIO_MAX_CHUNKS_PER_TICK:
		var count := mini(available, source_chunk)
		var frames := _capture.get_buffer(count)
		if frames.is_empty():
			break
		_send_audio(_resample_audio(frames))
		available -= frames.size()
		chunks += 1


## Converts the device/project mix rate to SPA1's fixed 48 kHz clock without
## resetting phase at capture-buffer boundaries. At Host-a's 96 kHz this emits
## one averaged output frame for each pair of source frames.
func _resample_audio(frames: PackedVector2Array) -> PackedVector2Array:
	if _audio_mix_rate == AUDIO_RATE:
		return frames
	var out := PackedVector2Array()
	for frame in frames:
		_audio_resample_sum += frame
		_audio_resample_count += 1
		_audio_resample_clock += AUDIO_RATE
		var emit := _audio_resample_clock / _audio_mix_rate
		if emit <= 0:
			continue
		_audio_resample_clock %= _audio_mix_rate
		var sample := _audio_resample_sum / float(_audio_resample_count)
		for _i in emit:
			out.append(sample)
		_audio_resample_sum = Vector2.ZERO
		_audio_resample_count = 0
	return out


func _send_audio(frames: PackedVector2Array) -> void:
	if _peer == null or frames.is_empty():
		return
	var pcm := PackedFloat32Array()
	pcm.resize(frames.size() * AUDIO_CHANNELS)
	for i in frames.size():
		pcm[i * 2] = frames[i].x
		pcm[i * 2 + 1] = frames[i].y
	var data := pcm.to_byte_array()
	var hdr := StreamPeerBuffer.new()
	hdr.big_endian = false
	hdr.put_data("SPA1".to_ascii_buffer())
	hdr.put_u32(AUDIO_RATE)
	hdr.put_u32(AUDIO_CHANNELS)
	hdr.put_u32(AUDIO_FORMAT_F32LE)
	hdr.put_u32(frames.size())
	hdr.put_u32(data.size())
	_peer.put_data(hdr.data_array)
	_peer.put_data(data)
	audio_chunks_sent += 1
