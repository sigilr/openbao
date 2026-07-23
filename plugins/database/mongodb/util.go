// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package mongodb

import "go.mongodb.org/mongo-driver/mongo/writeconcern"

type createUserCommand struct {
	Username string        `bson:"createUser"`
	Password string        `bson:"pwd,omitempty"`
	Roles    []interface{} `bson:"roles"`
}

type updateUserCommand struct {
	Username string `bson:"updateUser"`
	Password string `bson:"pwd"`
}

type dropUserCommand struct {
	Username     string                     `bson:"dropUser"`
	WriteConcern *writeconcern.WriteConcern `bson:"writeConcern"`
}

type mongodbRole struct {
	Role string `json:"role" bson:"role"`
	DB   string `json:"db"   bson:"db"`
}

type mongodbRoles []mongodbRole

type mongoDBStatement struct {
	DB    string       `json:"db"`
	Roles mongodbRoles `json:"roles"`
}

// toStandardRolesArray converts role documents into the MongoDB-native shape.
//
//	[ { "role": "readWrite" }, { "role": "readWrite", "db": "test" } ]
//
// becomes
//
//	[ "readWrite", { "role": "readWrite", "db": "test" } ]
//
// — bare role names are flattened to strings, db-qualified ones stay as
// documents, which is what createUser expects.
func (roles mongodbRoles) toStandardRolesArray() []interface{} {
	var standardRolesArray []interface{}
	for _, role := range roles {
		if role.DB == "" {
			standardRolesArray = append(standardRolesArray, role.Role)
		} else {
			standardRolesArray = append(standardRolesArray, role)
		}
	}
	return standardRolesArray
}
