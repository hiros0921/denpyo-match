# 監査ログ。読むだけ。
class AuditLogsController < ApplicationController
  def index
    @result = AuditLogQuery.new(
      organization_id: current_organization.id, params: filter_params
    ).call
    @actions = AuditLogQuery.available_actions(current_organization.id)
    @actors = current_organization.users.order(:id)

    # 表示中の範囲だけを検証する。
    #
    # 全件は行数に比例して伸びる（実測5万件で24ms＝100万件なら約0.5秒）。
    # 画面を開くたびに走らせる作りにすると、貯まるほど遅くなる。
    # 全件は「すべて検証する」を押したときだけ。
    @verify =
      if params[:verify_all] == "1"
        AuditLogQuery.verify
      elsif @result.rows.any?
        AuditLogQuery.verify(from_id: @result.rows.last.id, to_id: @result.rows.first.id)
      else
        { errors: [], elapsed_ms: 0.0 }
      end
    @verified_all = params[:verify_all] == "1"
  end

  private

  def filter_params
    params.permit(:action_name, :actor_id, :target_table, :target_id,
                  :from, :to, :cursor)
  end
end
