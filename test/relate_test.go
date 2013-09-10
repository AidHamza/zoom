package test

import (
	"github.com/stephenalexbrowne/zoom"
	"github.com/stephenalexbrowne/zoom/redis"
	"github.com/stephenalexbrowne/zoom/support"
	"github.com/stephenalexbrowne/zoom/util"
	"testing"
)

func TestSaveOneToOne(t *testing.T) {
	support.SetUp()
	defer support.TearDown()

	// create and save a new color
	c := &support.Color{R: 25, G: 152, B: 166}
	zoom.Save(c)

	// create and save a new artist, assigning favoriteColor to above
	a := &support.Artist{Name: "Alex", FavoriteColor: c}
	if err := zoom.Save(a); err != nil {
		t.Error(err)
	}

	// get a connection
	conn := zoom.GetConn()
	defer conn.Close()

	// invoke redis driver to check if the value was set appropriately
	colorKey := "artist:" + a.Id + ":FavoriteColor"
	id, err := redis.String(conn.Do("GET", colorKey))
	if err != nil {
		t.Error(err)
	}
	if id != c.Id {
		t.Errorf("color id for artist was not set correctly.\nExpected: %s\nGot: %s\n", c.Id, id)
	}
}

func TestFindOneToOne(t *testing.T) {
	support.SetUp()
	defer support.TearDown()

	// create and save a new color
	c := &support.Color{R: 25, G: 152, B: 166}
	zoom.Save(c)

	// create and save a new artist, assigning favoriteColor to above
	a := &support.Artist{Name: "Alex", FavoriteColor: c}
	zoom.Save(a)

	// find the saved person
	aCopy := &support.Artist{}
	if _, err := zoom.ScanById(aCopy, a.Id).Exec(); err != nil {
		t.Error(err)
	}

	// make sure favorite color is the same
	if aCopy.FavoriteColor == nil {
		t.Error("relation was not persisted. aCopy.FavoriteColor was nil")
	}
	if a.FavoriteColor.Id != aCopy.FavoriteColor.Id {
		t.Errorf("Id of favorite color was incorrect.\nExpected: %s\nGot: %s\n", a.FavoriteColor.Id, aCopy.FavoriteColor.Id)
	}
}

func TestSaveOneToMany(t *testing.T) {
	support.SetUp()
	defer support.TearDown()

	// create and save a new petOwner
	owners, err := support.CreatePetOwners(1)
	if err != nil {
		t.Error(err)
	}
	o := owners[0]

	// create and save some pets
	pets, err := support.CreatePets(3)
	if err != nil {
		t.Error(err)
	}

	// assign the pets to the owner
	o.Pets = pets
	if err := zoom.Save(o); err != nil {
		t.Error(err)
	}

	// get a connection
	conn := zoom.GetConn()
	defer conn.Close()

	// invoke redis driver to check if the value was set appropriately
	petsKey := "petOwner:" + o.Id + ":Pets"
	gotIds, err := redis.Strings(conn.Do("SMEMBERS", petsKey))
	if err != nil {
		t.Error(err)
	}

	// compare expected ids to got ids
	expectedIds := make([]string, 0)
	for _, pet := range o.Pets {
		if pet.Id == "" {
			t.Errorf("pet id was empty for %+v\n", pet)
		}
		expectedIds = append(expectedIds, pet.Id)
	}
	equal, msg := util.CompareAsStringSet(expectedIds, gotIds)
	if !equal {
		t.Errorf("pet ids were not correct.\n%s\n", msg)
	}
}

func TestFindOneToMany(t *testing.T) {
	support.SetUp()
	defer support.TearDown()

	// create and save a new petOwner
	owners, _ := support.CreatePetOwners(1)
	o := owners[0]

	// create and save some pets
	pets, _ := support.CreatePets(3)

	// assign the pets to the owner
	o.Pets = pets
	zoom.Save(o)

	// get a copy of the owner from the database
	oCopy := &support.PetOwner{}
	if _, err := zoom.ScanById(oCopy, o.Id).Exec(); err != nil {
		t.Error(err)
	}

	// compare expected ids to got ids
	expectedIds := make([]string, 0)
	for _, pet := range o.Pets {
		if pet.Id == "" {
			t.Errorf("pet id was empty for %+v\n", pet)
		}
		expectedIds = append(expectedIds, pet.Id)
	}
	gotIds := make([]string, 0)
	for _, pet := range oCopy.Pets {
		if pet.Id == "" {
			t.Errorf("pet id was empty for %+v\n", pet)
		}
		gotIds = append(gotIds, pet.Id)
	}
	equal, msg := util.CompareAsStringSet(expectedIds, gotIds)
	if !equal {
		t.Errorf("pet ids were not correct.\n%s\n", msg)
	}
}
