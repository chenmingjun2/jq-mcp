import { pageRun, serializeMutation } from "./page.js";

export const folderHandlers = {
  "folder.list": (p) => pageRun("folder.list", { fid: p.fid, recursive: !!p.recursive }),
  "folder.create": (p) =>
    serializeMutation(() => pageRun("folder.create", { name: p.name, parentId: p.parentId })),
};
