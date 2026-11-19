import { decode, encode, types } from "@oarkflow/codec";

interface User {
  ID: bigint;
  Name: string;
  Active: boolean;
}

const userSchema = types.struct({ ID: types.uint64, Name: types.string, Active: types.bool });
const message = encode(userSchema, { ID: 42n, Name: "Ada", Active: true }, { evolvable: true });
const user = decode<User>(userSchema, message);
console.log(user.Name, user.ID);
