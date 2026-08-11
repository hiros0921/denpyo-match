# 閾値シミュレーション。このプロダクトの核心。
#
# OCRの精度を上げることではなく、
# 「どの精度なら人の確認を省いてよいか」を現場が自分で決められることが売り。
class ThresholdsController < ApplicationController
  def index
    @current = Threshold.where(organization_id: current_organization.id)
                        .current.order(:client_id, :doc_type).to_a
    @clients = accessible_clients
    @upper = (params[:upper] || default_threshold.upper).to_f
    @lower = (params[:lower] || default_threshold.lower).to_f
    @client_id = params[:client_id].presence
    @result = simulator.simulate(upper: @upper, lower: @lower)
  end

  # スライダーを動かすたびに呼ばれる。照合はやり直さない。
  # 保存済みの match_candidates を集計するだけ。
  def simulate
    upper = params[:upper].to_f
    lower = params[:lower].to_f
    r = simulator.simulate(upper: upper, lower: lower)
    render partial: "thresholds/result",
           locals: { result: r, upper: upper, lower: lower }
  end

  # 適用。上書きせず、履歴として積む。
  #
  # 「この伝票はどの閾値設定で自動承認されたか」を後から辿る要件があるので、
  # 前の設定を消さない。valid_to を入れて世代を閉じ、新しい行を足す。
  def create
    unless current_user.can_edit_threshold?
      redirect_to thresholds_path, alert: "閾値を変えられるのは管理者だけです" and return
    end

    th = Threshold.new(
      organization_id: current_organization.id,
      client_id: params[:client_id].presence,
      doc_type: params[:doc_type].presence,
      upper: params[:upper], lower: params[:lower],
      created_by_id: current_user.id
    )
    unless th.valid?
      redirect_to thresholds_path, alert: th.errors.full_messages.join(" / ") and return
    end

    ActiveRecord::Base.transaction do
      Threshold.where(organization_id: current_organization.id,
                      client_id: params[:client_id].presence,
                      doc_type: params[:doc_type].presence,
                      valid_to: nil).update_all(valid_to: Time.current)
      th.save!
    end
    redirect_to thresholds_path,
                notice: "適用しました（前の設定は履歴として残っています）"
  end

  private

  def simulator
    ThresholdSimulator.new(organization_id: current_organization.id,
                           client_id: params[:client_id].presence,
                           doc_type: params[:doc_type].presence)
  end

  # DBに設定が無いときの出発点。Go 側の decide.Default と同じ値。
  # 第5段階の実測（正解の照合スコア平均88.2 / 不正解33.4、70で分離）にもとづく。
  def default_threshold
    Threshold.where(organization_id: current_organization.id).current
             .order(:client_id).first || Struct.new(:upper, :lower).new(95, 70)
  end
end
