module github.com/ezbuy/mgo

go 1.14

require (
	gopkg.in/check.v1 v1.0.0-20200227125254-8fa46927fb4f
	gopkg.in/mgo.v2 v2.0.0-00010101000000-000000000000
	gopkg.in/tomb.v2 v2.0.0-20161208151619-d5d1b5820637
	gopkg.in/yaml.v2 v2.2.8
)

replace gopkg.in/mgo.v2 => ../mgo
