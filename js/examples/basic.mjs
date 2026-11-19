import { decode, encode, types } from "@oarkflow/codec";

const User = types.struct({ ID: types.uint64, Name: types.string, Active: types.bool });
const message = encode(User, { ID: 42n, Name: "Ada", Active: true }, { evolvable: true });
const user = decode(User, message);
console.log(user);
