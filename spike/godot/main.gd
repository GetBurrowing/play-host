extends Node3D
## Spike scene: a spinning torus and a cube moving side to side, so a frozen
## or black stream is obvious in a grabbed frame.

var _torus: MeshInstance3D
var _cube: MeshInstance3D
var _t := 0.0


func _ready() -> void:
	DisplayServer.window_set_title("burrow-play-spike-01a0db5f")
	var env := WorldEnvironment.new()
	env.environment = Environment.new()
	env.environment.background_mode = Environment.BG_COLOR
	env.environment.background_color = Color(0.12, 0.14, 0.2)
	add_child(env)
	var cam := Camera3D.new()
	cam.position = Vector3(0, 1.5, 6)
	add_child(cam)
	cam.look_at(Vector3.ZERO)
	var sun := DirectionalLight3D.new()
	sun.rotation_degrees = Vector3(-50, 30, 0)
	add_child(sun)
	_torus = MeshInstance3D.new()
	_torus.mesh = TorusMesh.new()
	var m1 := StandardMaterial3D.new()
	m1.albedo_color = Color(0.9, 0.5, 0.1)
	_torus.material_override = m1
	add_child(_torus)
	_cube = MeshInstance3D.new()
	_cube.mesh = BoxMesh.new()
	var m2 := StandardMaterial3D.new()
	m2.albedo_color = Color(0.2, 0.8, 0.4)
	_cube.material_override = m2
	add_child(_cube)


func _process(delta: float) -> void:
	_t += delta
	_torus.rotation = Vector3(_t * 0.7, _t * 1.3, 0)
	_cube.position = Vector3(sin(_t * 2.0) * 2.5, -0.5, 1.0)
	_cube.rotation.y = _t * 3.0
