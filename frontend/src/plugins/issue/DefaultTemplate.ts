import { IssueCreate, UNKNOWN_ID } from "../../types";
import { IssueTemplate, TemplateContext } from "../types";

const template: IssueTemplate = {
  type: "bb.general",
  buildIssue: (
    ctx: TemplateContext
  ): Omit<IssueCreate, "projectId" | "creatorId"> => {
    return {
      name: "",
      type: "bb.general",
      description: "",
      pipeline: {
        stageList: [
          {
            name: "Troubleshoot database",
            environmentId: ctx.environmentList[0].id,
            type: "bb.stage.general",
            taskList: [
              {
                name: "Troubleshoot database",
                type: "bb.task.general",
                when: "MANUAL",
                databaseId: UNKNOWN_ID,
              },
            ],
          },
        ],
        creatorId: ctx.currentUser.id,
        name: "Create database pipeline",
      },
      payload: {},
    };
  },
  inputFieldList: [],
  outputFieldList: [],
};

export default template;
