-- Ninzo scooter profile for OSRM (W1, #183). A thin layer over OSRM's own car.lua from the pinned
-- image (ghcr.io/project-osrm/osrm-backend:v6.0.0), so only what differs for a city scooter is
-- written here. Every number marked "placeholder" is a starting guess until the road test (W8)
-- measures it; change PROFILE_VERSION whenever any rule or number changes.
--
-- On any OSRM upgrade, re-check that properties.traffic_signal_penalty is still read: OSRM marks
-- it deprecated (lib/obstacles.lua in v6.0.0 still uses it). The golden routes fail if it is not.

api_version = 4

-- car.lua and its lib/ live in /opt in the OSRM image.
package.path = '/opt/?.lua;' .. package.path
local car = require('car')

PROFILE_VERSION = 'scooter-2'

-- City speed cap for a delivery scooter, km/h (placeholder).
local SPEED_CAP = 40

-- City speeds for a scooter, km/h (placeholders). They are also a ceiling: a maxspeed tag can
-- lower a road's speed but never raise it above this table (car.lua would use maxspeed x 0.8).
-- Indian expressways bar two-wheelers: a motorway routes only when tagged motorcycle=yes (process_way).
local SPEEDS = {
  trunk          = 40,
  trunk_link     = 25,
  primary        = 35,
  primary_link   = 25,
  secondary      = 30,
  secondary_link = 20,
  tertiary       = 25,
  tertiary_link  = 20,
  unclassified   = 20,
  residential    = 18,
  living_street  = 8,
  service        = 10,
  motorway       = 40, -- only where two-wheelers are tagged as allowed
  motorway_link  = 25,
}

function setup()
  local p = car.setup()

  -- India drives on the left: a right turn crosses oncoming traffic, so car.lua's turn bias
  -- makes right turns the expensive ones.
  p.properties.left_hand_driving = true
  -- A U-turn across a median is slow and unsafe (placeholder, seconds).
  p.properties.u_turn_penalty = 60
  -- Average wait at a signal; car.lua uses 2 s (placeholder, seconds). A red light in Bengaluru
  -- can be 60 to 120 s.
  p.properties.traffic_signal_penalty = 30
  -- Largest penalty for a sharp turn, and how much harder a turn across traffic is (placeholders).
  p.turn_penalty = 12
  p.turn_bias = 1.3

  -- A scooter, not a car: narrow lanes and low barriers are fine.
  p.vehicle_height = 1.4
  p.vehicle_width = 0.9
  p.vehicle_length = 2.0
  p.vehicle_weight = 250

  -- Two-wheeler tags first. In OpenStreetMap a scooter over 50 cc is a motorcycle. The same list
  -- decides which oneway:<mode> and turn-restriction tags apply.
  p.access_tags_hierarchy = Sequence { 'motorcycle', 'motor_vehicle', 'vehicle', 'access' }
  p.restrictions = Sequence { 'motorcycle', 'motor_vehicle', 'vehicle' }

  -- Motorways are in the table so that one tagged for two-wheelers routes; process_way drops the
  -- rest before car.lua sees them.
  p.speeds = Sequence { highway = SPEEDS }
  p.restricted_highway_whitelist = Set {
    'motorway', 'motorway_link',
    'trunk', 'trunk_link', 'primary', 'primary_link', 'secondary', 'secondary_link',
    'tertiary', 'tertiary_link', 'residential', 'living_street', 'unclassified', 'service',
  }
  return p
end

local function ceiling(speed, limit)
  if speed and speed > limit then
    return limit
  end
  return speed
end

function process_way(profile, way, result, relations)
  local highway = way:get_value_by_key('highway')
  local motorcycle = way:get_value_by_key('motorcycle')
  local motorway = highway == 'motorway' or highway == 'motorway_link'
  -- A motorway only when two-wheelers are tagged as allowed on it.
  if motorway and motorcycle ~= 'yes' and motorcycle ~= 'designated' then
    return
  end
  car.process_way(profile, way, result, relations)
  -- Never faster than this road type's scooter speed, nor the city cap, whatever maxspeed says.
  local limit = math.min(SPEEDS[highway] or SPEED_CAP, SPEED_CAP)
  result.forward_speed = ceiling(result.forward_speed, limit)
  result.backward_speed = ceiling(result.backward_speed, limit)
  if result.forward_rate and result.forward_rate > limit / 3.6 then
    result.forward_rate = limit / 3.6
  end
  if result.backward_rate and result.backward_rate > limit / 3.6 then
    result.backward_rate = limit / 3.6
  end
end

return {
  setup = setup,
  process_way = process_way,
  process_node = car.process_node,
  process_turn = car.process_turn,
}
